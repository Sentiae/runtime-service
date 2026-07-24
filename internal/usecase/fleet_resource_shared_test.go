package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

type fakeLogical struct {
	provisioned []LogicalProvisionRequest
	dropped     [][2]string
	provErr     error
	dropErr     error
}

func (f *fakeLogical) ProvisionLogical(_ context.Context, in LogicalProvisionRequest) (LogicalLease, error) {
	f.provisioned = append(f.provisioned, in)
	if f.provErr != nil {
		return LogicalLease{}, f.provErr
	}
	return LogicalLease{DBName: in.DBName, RoleName: in.RoleName}, nil
}

func (f *fakeLogical) DropLogical(_ context.Context, dbName, roleName string) error {
	f.dropped = append(f.dropped, [2]string{dbName, roleName})
	return f.dropErr
}

func testSharedCfg() SharedEngineConfig {
	return SharedEngineConfig{Host: "shared-pg", Port: 5432, TTL: time.Hour, SeedTemplates: []string{"tmpl_app"}}
}

func validSharedInput() ProvisionSharedInput {
	return ProvisionSharedInput{
		OwnerOrg:     uuid.New().String(),
		ClaimKey:     "cache-db",
		Env:          "prod",
		Revision:     1,
		Class:        "postgres",
		Tier:         "shared",
		SecretRefs:   []string{"secret/data/pg#password"},
		VaultToken:   "vault-token",
		SeedTemplate: "tmpl_app",
	}
}

func TestProvisionShared_Validation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ProvisionSharedInput)
		wantErr error
	}{
		{"wrong class", func(in *ProvisionSharedInput) { in.Class = "redis" }, domain.ErrResourceClassUnsupported},
		{"wrong tier", func(in *ProvisionSharedInput) { in.Tier = "dedicated" }, domain.ErrResourceTierUnsupported},
		{"missing owner", func(in *ProvisionSharedInput) { in.OwnerOrg = "" }, domain.ErrResourceOwnerOrgRequired},
		{"missing claim key", func(in *ProvisionSharedInput) { in.ClaimKey = "" }, domain.ErrResourceClaimKeyRequired},
		{"missing secrets", func(in *ProvisionSharedInput) { in.SecretRefs = nil }, domain.ErrResourceSecretsRequired},
		{"missing vault token", func(in *ProvisionSharedInput) { in.VaultToken = "" }, domain.ErrResourceVaultTokenRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			logical := &fakeLogical{}
			uc := NewFleetResourceSharedProvisioner(logical, repo, testSharedCfg())
			in := validSharedInput()
			tt.mutate(&in)
			_, err := uc.ProvisionShared(context.Background(), in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
			if len(logical.provisioned) != 0 {
				t.Fatalf("logical provision must not run on validation failure")
			}
		})
	}
}

func TestProvisionShared_IdempotentSameRevision(t *testing.T) {
	repo := newFakeResourceRepo()
	logical := &fakeLogical{}
	uc := NewFleetResourceSharedProvisioner(logical, repo, testSharedCfg())

	in := validSharedInput()
	owner := uuid.MustParse(in.OwnerOrg)
	existingID := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: existingID, OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
		Revision: 1, Tier: "shared", Phase: domain.FleetResourcePhaseReady, Endpoint: "shared-pg:5432",
	})

	out, err := uc.ProvisionShared(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if out.Handle != existingID.String() {
		t.Errorf("handle = %q, want %q", out.Handle, existingID)
	}
	if len(logical.provisioned) != 0 {
		t.Errorf("declarative ensure must not re-provision logical db")
	}
}

func TestProvisionShared_ConvergeRejected(t *testing.T) {
	repo := newFakeResourceRepo()
	logical := &fakeLogical{}
	uc := NewFleetResourceSharedProvisioner(logical, repo, testSharedCfg())

	in := validSharedInput()
	in.Revision = 3
	owner := uuid.MustParse(in.OwnerOrg)
	repo.seed(&domain.FleetResource{
		ID: uuid.New(), OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
		Revision: 1, Tier: "shared", Phase: domain.FleetResourcePhaseReady,
	})

	_, err := uc.ProvisionShared(context.Background(), in)
	if !errors.Is(err, domain.ErrResourceConvergeNotSupported) {
		t.Fatalf("got %v, want ErrResourceConvergeNotSupported", err)
	}
	if len(logical.provisioned) != 0 {
		t.Errorf("converge reject must not provision logical db")
	}
}

func TestReapOnce_DropsAndTombstones(t *testing.T) {
	repo := newFakeResourceRepo()
	logical := &fakeLogical{}
	uc := NewFleetResourceSharedProvisioner(logical, repo, testSharedCfg())

	past := time.Now().Add(-time.Hour)
	rid := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
		Tier: "shared", Phase: domain.FleetResourcePhaseReady,
		DBName: "res_c_abc", RoleName: "r_deadbeef", ExpiresAt: &past,
	})

	uc.reapOnce(context.Background())

	if len(logical.dropped) != 1 || logical.dropped[0] != [2]string{"res_c_abc", "r_deadbeef"} {
		t.Fatalf("dropped = %v, want one drop of res_c_abc/r_deadbeef", logical.dropped)
	}
	row, _ := repo.GetResourceByHandle(context.Background(), rid)
	if row.Phase != domain.FleetResourcePhaseDecommissioned || row.DecommissionedAt == nil {
		t.Errorf("expired row not tombstoned: phase=%q at=%v", row.Phase, row.DecommissionedAt)
	}
}

func TestReapOnce_DropFailureLeavesRowLive(t *testing.T) {
	repo := newFakeResourceRepo()
	logical := &fakeLogical{dropErr: errors.New("pg unreachable")}
	uc := NewFleetResourceSharedProvisioner(logical, repo, testSharedCfg())

	past := time.Now().Add(-time.Hour)
	rid := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
		Tier: "shared", Phase: domain.FleetResourcePhaseReady,
		DBName: "res_c_abc", RoleName: "r_deadbeef", ExpiresAt: &past,
	})

	uc.reapOnce(context.Background())

	row, _ := repo.GetResourceByHandle(context.Background(), rid)
	if row.Phase == domain.FleetResourcePhaseDecommissioned {
		t.Errorf("row must stay live when drop fails so the next tick retries")
	}
}

func TestPickSharedPassword(t *testing.T) {
	tests := []struct {
		name    string
		secrets []HostSecret
		want    string
		wantErr error
	}{
		{"single", []HostSecret{{Name: "dsn", Val: "pw1"}}, "pw1", nil},
		{"named password", []HostSecret{{Name: "user", Val: "u"}, {Name: "Password", Val: "pw2"}}, "pw2", nil},
		{"ambiguous", []HostSecret{{Name: "a", Val: "x"}, {Name: "b", Val: "y"}}, "", domain.ErrResourceSharedPasswordAmbiguous},
		{"empty", nil, "", domain.ErrResourceSharedPasswordAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickSharedPassword(tt.secrets)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("password = %q, want %q", got, tt.want)
			}
		})
	}
}
