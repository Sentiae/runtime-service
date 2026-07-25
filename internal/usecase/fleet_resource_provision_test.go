package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────

var errFakeResourceDuplicate = errors.New("fake: duplicate claim")

type fakeResourceRepo struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]*domain.FleetResource
	byClaim  map[string]*domain.FleetResource
	recovery map[uuid.UUID][]domain.FleetResourceRecoveryPoint

	// knobs
	findNotFoundFirst bool
	findCalls         int
	saveDuplicate     bool
	casErr            error
}

func newFakeResourceRepo() *fakeResourceRepo {
	return &fakeResourceRepo{
		byID:     map[uuid.UUID]*domain.FleetResource{},
		byClaim:  map[string]*domain.FleetResource{},
		recovery: map[uuid.UUID][]domain.FleetResourceRecoveryPoint{},
	}
}

func claimKey(owner uuid.UUID, claim, env string) string {
	return owner.String() + "|" + claim + "|" + env
}

func (f *fakeResourceRepo) seed(r *domain.FleetResource) {
	cp := *r
	f.byID[r.ID] = &cp
	f.byClaim[claimKey(r.OwnerOrg, r.ClaimKey, r.Env)] = &cp
}

func (f *fakeResourceRepo) SaveResource(_ context.Context, r *domain.FleetResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveDuplicate {
		if existing, ok := f.byClaim[claimKey(r.OwnerOrg, r.ClaimKey, r.Env)]; ok && existing.ID != r.ID {
			return errFakeResourceDuplicate
		}
	}
	cp := *r
	f.byID[r.ID] = &cp
	f.byClaim[claimKey(r.OwnerOrg, r.ClaimKey, r.Env)] = &cp
	return nil
}

func (f *fakeResourceRepo) GetResourceByHandle(_ context.Context, id uuid.UUID) (*domain.FleetResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.byID[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrResourceNotFound
}

func (f *fakeResourceRepo) FindResource(_ context.Context, owner uuid.UUID, claim, env string) (*domain.FleetResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findCalls++
	if f.findNotFoundFirst && f.findCalls == 1 {
		return nil, domain.ErrResourceNotFound
	}
	if r, ok := f.byClaim[claimKey(owner, claim, env)]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrResourceNotFound
}

func (f *fakeResourceRepo) UpdateResourcePhase(_ context.Context, id uuid.UUID, phase domain.FleetResourcePhase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return domain.ErrResourceNotFound
	}
	r.Phase = phase
	return nil
}

// CompareAndSwapPhase mirrors the postgres UPDATE ... WHERE id = ? AND phase IN
// (?): it advances the phase only from one of `from` and reports whether a row
// changed.
func (f *fakeResourceRepo) CompareAndSwapPhase(_ context.Context, id uuid.UUID, from []domain.FleetResourcePhase, to domain.FleetResourcePhase) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.casErr != nil {
		return false, f.casErr
	}
	r, ok := f.byID[id]
	if !ok {
		return false, nil
	}
	for _, p := range from {
		if r.Phase == p {
			r.Phase = to
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeResourceRepo) SetResourceLastError(_ context.Context, id uuid.UUID, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return domain.ErrResourceNotFound
	}
	r.LastError = msg
	return nil
}

func (f *fakeResourceRepo) ListResourcesByPhase(_ context.Context, phase domain.FleetResourcePhase) ([]domain.FleetResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.FleetResource
	for _, r := range f.byID {
		if r.Phase == phase && r.DecommissionedAt == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// GetRecoveryPointByRef filters on BOTH resource_id and object_key, exactly like
// the postgres repo — a ref from another resource must not resolve.
func (f *fakeResourceRepo) GetRecoveryPointByRef(_ context.Context, resourceID uuid.UUID, objectKey string) (*domain.FleetResourceRecoveryPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rp := range f.recovery[resourceID] {
		if rp.ObjectKey == objectKey {
			cp := rp
			return &cp, nil
		}
	}
	return nil, domain.ErrRecoveryPointNotFound
}

func (f *fakeResourceRepo) MarkRecoveryPointRestoredInPlace(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for resID := range f.recovery {
		for i := range f.recovery[resID] {
			if f.recovery[resID][i].ID == id {
				f.recovery[resID][i].RestoredInPlaceOK = true
				return nil
			}
		}
	}
	return domain.ErrRecoveryPointNotFound
}

func (f *fakeResourceRepo) ListExpiredShared(_ context.Context, now time.Time) ([]domain.FleetResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.FleetResource
	for _, r := range f.byID {
		if r.AppID == nil && r.ExpiresAt != nil && !r.ExpiresAt.After(now) && r.DecommissionedAt == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeResourceRepo) SaveRecoveryPoint(_ context.Context, rp *domain.FleetResourceRecoveryPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovery[rp.ResourceID] = append([]domain.FleetResourceRecoveryPoint{*rp}, f.recovery[rp.ResourceID]...)
	return nil
}

func (f *fakeResourceRepo) ListRecoveryPoints(_ context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.FleetResourceRecoveryPoint(nil), f.recovery[resourceID]...), nil
}

type fakeFleetProvisioner struct {
	provisionCalls  int
	lastInput       FleetProvisionInput
	provisionOut    FleetProvisionOutput
	provisionErr    error
	healthCalls     int
	healthOut       FleetHealthOutput
	healthErr       error
	decommissioned  []string
	decommissionErr error
}

func (f *fakeFleetProvisioner) Provision(_ context.Context, in FleetProvisionInput) (FleetProvisionOutput, error) {
	f.provisionCalls++
	f.lastInput = in
	if f.provisionErr != nil {
		return FleetProvisionOutput{}, f.provisionErr
	}
	return f.provisionOut, nil
}

func (f *fakeFleetProvisioner) Health(_ context.Context, _ string) (FleetHealthOutput, error) {
	f.healthCalls++
	return f.healthOut, f.healthErr
}

func (f *fakeFleetProvisioner) Decommission(_ context.Context, handle string) error {
	f.decommissioned = append(f.decommissioned, handle)
	return f.decommissionErr
}

type fakeSnapshotter struct {
	calls      int
	resourceID uuid.UUID
	appID      uuid.UUID
	err        error
	// noVolumes reproduces the real snapshotter's zero-volume answer: the call
	// SUCCEEDS and creates nothing.
	noVolumes bool
	// produced records the recovery points handed back, so a test can assert the
	// caller reports the SAME one it was given.
	produced []domain.FleetResourceRecoveryPoint
}

func (f *fakeSnapshotter) SnapshotAppVolumes(_ context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	f.calls++
	f.resourceID = resourceID
	f.appID = appID
	if f.err != nil {
		return nil, f.err
	}
	if f.noVolumes {
		return []domain.FleetResourceRecoveryPoint{}, nil
	}
	f.produced = []domain.FleetResourceRecoveryPoint{{
		ID: uuid.New(), ResourceID: resourceID, ObjectKey: "volumes/v1/final.ext4",
		Kind: "snapshot", CreatedAt: time.Now().UTC(),
	}}
	return f.produced, nil
}

// fakeResourceReplicaRepo serves FindByID + ListByApp from in-memory maps
// (distinct from the scheduler test's fakeReplicaRepo).
type fakeResourceReplicaRepo struct {
	byApp map[uuid.UUID][]domain.Replica
	byID  map[uuid.UUID]*domain.Replica
	// listErr makes ListByApp fail, which a readiness gate must treat as "cannot
	// be proven usable" rather than as "nothing to probe".
	listErr error
}

func newFakeResourceReplicaRepo() *fakeResourceReplicaRepo {
	return &fakeResourceReplicaRepo{
		byApp: map[uuid.UUID][]domain.Replica{},
		byID:  map[uuid.UUID]*domain.Replica{},
	}
}

func (f *fakeResourceReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (f *fakeResourceReplicaRepo) Update(_ context.Context, r *domain.Replica) error {
	cp := *r
	f.byID[r.ID] = &cp
	return nil
}
func (f *fakeResourceReplicaRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Replica, error) {
	if r, ok := f.byID[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrReplicaNotFound
}
func (f *fakeResourceReplicaRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Replica, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byApp[appID], nil
}
func (f *fakeResourceReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *fakeResourceReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (f *fakeResourceReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }

func testEngine() DedicatedEngineConfig {
	return DedicatedEngineConfig{Registry: "reg", Repository: "sentiae/pg", Digest: "sha256:abc", ConnBudget: 100}
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func validDedicatedInput() ProvisionDedicatedInput {
	return ProvisionDedicatedInput{
		OwnerOrg:   uuid.New().String(),
		ClaimKey:   "orders-db",
		Env:        "prod",
		Revision:   1,
		Class:      "postgres",
		Tier:       "dedicated",
		SecretRefs: []string{"secret/data/pg#password"},
		VaultToken: "vault-token",
		SizeMB:     1024,
	}
}

func TestProvisionDedicated_Validation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ProvisionDedicatedInput)
		wantErr error
	}{
		{"wrong class", func(in *ProvisionDedicatedInput) { in.Class = "redis" }, domain.ErrResourceClassUnsupported},
		{"wrong tier", func(in *ProvisionDedicatedInput) { in.Tier = "shared" }, domain.ErrResourceTierUnsupported},
		{"missing owner", func(in *ProvisionDedicatedInput) { in.OwnerOrg = "" }, domain.ErrResourceOwnerOrgRequired},
		{"missing claim key", func(in *ProvisionDedicatedInput) { in.ClaimKey = "" }, domain.ErrResourceClaimKeyRequired},
		{"missing secrets", func(in *ProvisionDedicatedInput) { in.SecretRefs = nil }, domain.ErrResourceSecretsRequired},
		{"missing vault token", func(in *ProvisionDedicatedInput) { in.VaultToken = "" }, domain.ErrResourceVaultTokenRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{}
			uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())
			in := validDedicatedInput()
			tt.mutate(&in)
			_, err := uc.ProvisionDedicated(context.Background(), in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
			if prov.provisionCalls != 0 {
				t.Fatalf("provision must not run on validation failure (calls=%d)", prov.provisionCalls)
			}
		})
	}
}

func TestProvisionDedicated_EngineUnconfigured(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, DedicatedEngineConfig{ConnBudget: 100})
	_, err := uc.ProvisionDedicated(context.Background(), validDedicatedInput())
	if !errors.Is(err, domain.ErrImageRefIncomplete) {
		t.Fatalf("got %v, want ErrImageRefIncomplete", err)
	}
}

func TestProvisionDedicated_HappyPath(t *testing.T) {
	repo := newFakeResourceRepo()
	appHandle := uuid.New()
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: appHandle.String()}}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())

	in := validDedicatedInput()
	out, err := uc.ProvisionDedicated(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if prov.provisionCalls != 1 {
		t.Fatalf("provision calls = %d, want 1", prov.provisionCalls)
	}
	// Descriptor shape.
	gi := prov.lastInput
	if gi.ComponentID != "resource/"+in.OwnerOrg+"/orders-db" {
		t.Errorf("component_id = %q", gi.ComponentID)
	}
	if gi.WorkloadClass != string(domain.ImageWorkloadClassResident) {
		t.Errorf("class = %q, want resident", gi.WorkloadClass)
	}
	if gi.Port != 5432 {
		t.Errorf("port = %d, want 5432", gi.Port)
	}
	if len(gi.Volumes) != 1 || gi.Volumes[0].MountPath != "/data" || gi.Volumes[0].SizeMB != 1024 {
		t.Errorf("volumes = %+v", gi.Volumes)
	}
	if gi.MinReplicas != 1 || gi.MaxReplicas != 1 || gi.ScaleToZero {
		t.Errorf("replica bounds min=%d max=%d s2z=%v", gi.MinReplicas, gi.MaxReplicas, gi.ScaleToZero)
	}
	if gi.EnvVars["PGDATA"] != "/data/pgdata" {
		t.Errorf("PGDATA = %q", gi.EnvVars["PGDATA"])
	}
	if gi.Registry != "reg" || gi.Repository != "sentiae/pg" || gi.Digest != "sha256:abc" {
		t.Errorf("engine image = %s/%s@%s", gi.Registry, gi.Repository, gi.Digest)
	}
	if gi.VaultToken != "vault-token" {
		t.Errorf("vault token not propagated")
	}
	// Persisted row.
	rid, err := uuid.Parse(out.Handle)
	if err != nil {
		t.Fatalf("bad handle: %v", err)
	}
	row, err := repo.GetResourceByHandle(context.Background(), rid)
	if err != nil {
		t.Fatalf("row not persisted: %v", err)
	}
	if row.Tier != "dedicated" || row.Phase != domain.FleetResourcePhaseProvisioning {
		t.Errorf("row tier=%q phase=%q", row.Tier, row.Phase)
	}
	if row.AppID == nil || *row.AppID != appHandle {
		t.Errorf("app_id = %v, want %v", row.AppID, appHandle)
	}
}

// TestProvisionDedicated_IdempotentSameRevision pins the declarative-ensure
// contract of #p19-handed-token-not-rehandable: a same-revision re-provision
// re-drives the existing app (which is what re-hands the memory-only Vault
// token), and it does so WITHOUT ever changing the frozen Handle/Phase response
// or minting a second app. The refusal cases matter as much as the recovery one:
// re-provisioning a claim whose app row is gone would upsert a brand-new app —
// and a brand-new EMPTY volume — behind a resource row still pointing at the
// vanished app.
func TestProvisionDedicated_IdempotentSameRevision(t *testing.T) {
	appID := uuid.New()

	tests := []struct {
		name          string
		revision      int
		existingPhase domain.FleetResourcePhase
		appID         *uuid.UUID
		healthErr     error
		wantErr       error
		wantHealth    int
		wantProvision int
	}{
		{
			name:          "live claim is re-driven so the token is re-handed",
			revision:      1,
			existingPhase: domain.FleetResourcePhaseReady,
			appID:         &appID,
			wantHealth:    1,
			wantProvision: 1,
		},
		{
			name:          "provisioning claim is re-driven too",
			revision:      1,
			existingPhase: domain.FleetResourcePhaseProvisioning,
			appID:         &appID,
			wantHealth:    1,
			wantProvision: 1,
		},
		{
			name:          "tombstone is never re-booted",
			revision:      1,
			existingPhase: domain.FleetResourcePhaseDecommissioned,
			appID:         &appID,
			wantHealth:    0,
			wantProvision: 0,
		},
		{
			name:          "claim with no backing app cannot be recovered here",
			revision:      1,
			existingPhase: domain.FleetResourcePhaseReady,
			appID:         nil,
			wantHealth:    0,
			wantProvision: 0,
		},
		{
			name:          "vanished app row refuses rather than minting a new one",
			revision:      1,
			existingPhase: domain.FleetResourcePhaseReady,
			appID:         &appID,
			healthErr:     domain.ErrWorkloadNotFound,
			wantHealth:    1,
			wantProvision: 0,
		},
		{
			name:          "different revision still rejects converge",
			revision:      2,
			existingPhase: domain.FleetResourcePhaseReady,
			appID:         &appID,
			wantErr:       domain.ErrResourceConvergeNotSupported,
			wantHealth:    0,
			wantProvision: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{
				healthErr:    tt.healthErr,
				provisionOut: FleetProvisionOutput{Handle: appID.String()},
			}
			uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())

			in := validDedicatedInput()
			in.Revision = tt.revision
			owner := uuid.MustParse(in.OwnerOrg)
			existingID := uuid.New()
			repo.seed(&domain.FleetResource{
				ID: existingID, OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
				Revision: 1, Tier: "dedicated", Phase: tt.existingPhase, AppID: tt.appID,
			})

			out, err := uc.ProvisionDedicated(context.Background(), in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("provision: %v", err)
				}
				// The frozen P19 response shape: the claim's own handle + its stored
				// phase, identical on every idempotent path.
				if out.Handle != existingID.String() {
					t.Errorf("handle = %q, want existing %q", out.Handle, existingID)
				}
				if out.Phase != string(tt.existingPhase) {
					t.Errorf("phase = %q, want %q", out.Phase, tt.existingPhase)
				}
			}
			if prov.healthCalls != tt.wantHealth {
				t.Errorf("health calls = %d, want %d", prov.healthCalls, tt.wantHealth)
			}
			if prov.provisionCalls != tt.wantProvision {
				t.Errorf("provision calls = %d, want %d", prov.provisionCalls, tt.wantProvision)
			}
			if tt.wantProvision > 0 {
				// The recovery must hand back the SAME descriptor the claim was created
				// from — including the freshly supplied token — never a drifted one.
				if got := prov.lastInput; got.ComponentID != "resource/"+in.OwnerOrg+"/"+in.ClaimKey ||
					got.VaultToken != in.VaultToken || got.Port != residentPGPort {
					t.Errorf("recovery descriptor = %+v", got)
				}
			}
			// A recovery failure must never surface as a provision failure or leave a
			// mutated row: the caller polls this handle.
			row, gerr := repo.GetResourceByHandle(context.Background(), existingID)
			if gerr != nil {
				t.Fatalf("row lookup: %v", gerr)
			}
			if row.Phase != tt.existingPhase || row.Revision != 1 {
				t.Errorf("row mutated: phase=%q revision=%d", row.Phase, row.Revision)
			}
		})
	}
}

func TestProvisionDedicated_DuplicateRaceReturnsWinner(t *testing.T) {
	repo := newFakeResourceRepo()
	repo.findNotFoundFirst = true // pre-check sees no claim
	repo.saveDuplicate = true     // SaveResource collides with the winner
	prov := &fakeFleetProvisioner{provisionOut: FleetProvisionOutput{Handle: uuid.New().String()}}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())

	in := validDedicatedInput()
	owner := uuid.MustParse(in.OwnerOrg)
	winnerID := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: winnerID, OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
		Revision: 1, Tier: "dedicated", Phase: domain.FleetResourcePhaseProvisioning,
	})

	out, err := uc.ProvisionDedicated(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if out.Handle != winnerID.String() {
		t.Fatalf("handle = %q, want winner %q", out.Handle, winnerID)
	}
}

func TestStatusOf_HealthyBecomesReady(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
	appID := uuid.New()
	replicas := newFakeResourceReplicaRepo()
	guestReplica := domain.Replica{ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident, GuestIP: "10.0.0.9", Port: residentPGPort}
	replicas.byApp[appID] = []domain.Replica{guestReplica}
	uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())
	uc.pgReady = func(context.Context, string, int) error { return nil }

	rid := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
		Tier: "dedicated", Phase: domain.FleetResourcePhaseProvisioning, AppID: &appID,
		SecretRefs: []string{"secret/data/pg#password"},
	})
	repo.recovery[rid] = []domain.FleetResourceRecoveryPoint{{ID: uuid.New(), ResourceID: rid, ObjectKey: "volumes/x/y.ext4"}}

	st, err := uc.StatusOf(context.Background(), rid)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != string(domain.FleetResourcePhaseReady) {
		t.Errorf("phase = %q, want ready", st.Phase)
	}
	if st.Endpoint != "10.0.0.9:5432" {
		t.Errorf("endpoint = %q, want 10.0.0.9:5432", st.Endpoint)
	}
	if st.ConnBudget != 100 {
		t.Errorf("conn budget = %d, want 100", st.ConnBudget)
	}
	if st.LastRecoveryPoint == nil || st.LastRecoveryPoint.ObjectKey != "volumes/x/y.ext4" {
		t.Errorf("last recovery point = %+v", st.LastRecoveryPoint)
	}
}

// D-184 — a healthy observation must NOT auto-advance a resource whose restore
// is still running: the OLD engine is healthy right up to the drain, and the
// restored one is not proven until the restore says so.
func TestStatusOf_DoesNotAdvanceWhileRestoring(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
	appID := uuid.New()
	replicas := newFakeResourceReplicaRepo()
	uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())
	uc.pgReady = func(context.Context, string, int) error { return nil }

	rid := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
		Tier: "dedicated", Phase: domain.FleetResourcePhaseRestoring, AppID: &appID,
	})

	st, err := uc.StatusOf(context.Background(), rid)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Phase != string(domain.FleetResourcePhaseRestoring) {
		t.Errorf("reported phase = %q, want restoring", st.Phase)
	}
	stored, _ := repo.GetResourceByHandle(context.Background(), rid)
	if stored.Phase != domain.FleetResourcePhaseRestoring {
		t.Errorf("stored phase = %q, want restoring (never auto-advanced)", stored.Phase)
	}
}

// A resource whose health cannot be read is a STATE, not an API error: the
// status call is the only way an operator sees a stuck resource, and hard-
// erroring turned "your database is stuck" into "the API is broken".
func TestStatusOf_UnreadableHealthIsAConditionNotAnError(t *testing.T) {
	tests := []struct {
		name          string
		healthErr     error
		wantCondition string
	}{
		{
			name:          "backing app is gone",
			healthErr:     domain.ErrWorkloadNotFound,
			wantCondition: conditionBackingAppMissing,
		},
		{
			name:          "app row lookup says not found",
			healthErr:     fmt.Errorf("load app: %w", domain.ErrFleetAppNotFound),
			wantCondition: conditionBackingAppMissing,
		},
		{
			name:          "anything else is unreadable, not missing",
			healthErr:     errors.New("connection refused"),
			wantCondition: conditionHealthUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{healthErr: tt.healthErr}
			appID := uuid.New()
			replicas := newFakeResourceReplicaRepo()
			uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())

			rid := uuid.New()
			repo.seed(&domain.FleetResource{
				ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
				Tier: "dedicated", Phase: domain.FleetResourcePhaseProvisioning, AppID: &appID,
			})
			repo.recovery[rid] = []domain.FleetResourceRecoveryPoint{{ID: uuid.New(), ResourceID: rid, ObjectKey: "volumes/x/y.ext4"}}

			st, err := uc.StatusOf(context.Background(), rid)
			if err != nil {
				t.Fatalf("a failed health probe must not fail the status call: %v", err)
			}
			if len(st.Conditions) != 1 || st.Conditions[0] != tt.wantCondition {
				t.Fatalf("conditions = %v, want [%s]", st.Conditions, tt.wantCondition)
			}
			if st.Phase != string(domain.FleetResourcePhaseProvisioning) {
				t.Errorf("phase = %q, want the durable phase unchanged", st.Phase)
			}
			// The recovery catalog is exactly what a recovery is built from, so it
			// must still be reported when health cannot be read.
			if st.LastRecoveryPoint == nil {
				t.Error("recovery points must still be reported for an unhealthy resource")
			}
			stored, _ := repo.GetResourceByHandle(context.Background(), rid)
			if stored.Phase != domain.FleetResourcePhaseProvisioning {
				t.Errorf("stored phase = %q, want it untouched", stored.Phase)
			}
		})
	}
}

// #p19-restore-false-green-health — process-alive + a TCP dial is NOT readiness.
// A Postgres whose pg_hba.conf came back torn passes both while refusing every
// client, so `ready` (reported AND persisted) additionally requires a resident
// replica to ADMIT a connection. Every way of NOT being able to prove that is a
// refusal, because the alternative is a false green over customer data.
func TestStatusOf_ReadyRequiresTheEngineToAdmitClients(t *testing.T) {
	admits := func(context.Context, string, int) error { return nil }
	refuses := func(context.Context, string, int) error {
		return errors.New(`postgres at 10.0.0.9:5432 refused the connection before authentication: FATAL: SQLSTATE 28000: no pg_hba.conf entry for host "10.0.0.1"`)
	}
	appID := uuid.New()
	resident := []domain.Replica{{
		ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
		GuestIP: "10.0.0.9", Port: residentPGPort,
	}}
	addressless := []domain.Replica{{
		ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
	}}
	paused := []domain.Replica{{
		ID: uuid.New(), AppID: appID, State: domain.ReplicaStatePaused,
		GuestIP: "10.0.0.9", Port: residentPGPort,
	}}

	tests := []struct {
		name           string
		stored         domain.FleetResourcePhase
		replicas       []domain.Replica
		listErr        error
		probe          func(context.Context, string, int) error
		noProbe        bool
		wantPhase      domain.FleetResourcePhase
		wantConditions []string
		wantStored     domain.FleetResourcePhase
	}{
		{
			name: "engine admits: promoted and persisted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: resident, probe: admits,
			wantPhase: domain.FleetResourcePhaseReady, wantStored: domain.FleetResourcePhaseReady,
		},
		{
			name: "engine refuses: degraded and ready is never persisted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: resident, probe: refuses,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseProvisioning,
		},
		{
			name: "already ready and now refusing: reported degraded, phase NOT demoted", stored: domain.FleetResourcePhaseReady,
			replicas: resident, probe: refuses,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseReady,
		},
		{
			name: "no probe wired: cannot be proven, so not promoted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: resident, noProbe: true,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseProvisioning,
		},
		{
			name: "no resident replica to probe: not promoted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: paused, probe: admits,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseProvisioning,
		},
		{
			name: "resident replica with no guest address: not promoted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: addressless, probe: admits,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseProvisioning,
		},
		{
			name: "replica listing fails: not promoted", stored: domain.FleetResourcePhaseProvisioning,
			replicas: resident, listErr: errors.New("fake: replica store down"), probe: admits,
			wantPhase: domain.FleetResourcePhaseDegraded, wantConditions: []string{conditionEngineNotAdmitting},
			wantStored: domain.FleetResourcePhaseProvisioning,
		},
		{
			// D-184 — a restore owns the phase while it runs; the probe verdict, either
			// way, must not touch it.
			name: "restoring: untouched regardless of the probe", stored: domain.FleetResourcePhaseRestoring,
			replicas: resident, probe: refuses,
			wantPhase: domain.FleetResourcePhaseRestoring, wantStored: domain.FleetResourcePhaseRestoring,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			prov := &fakeFleetProvisioner{healthOut: FleetHealthOutput{Healthy: true}}
			replicas := newFakeResourceReplicaRepo()
			replicas.byApp[appID] = tt.replicas
			replicas.listErr = tt.listErr
			uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())
			if tt.noProbe {
				uc.pgReady = nil
			} else {
				uc.pgReady = tt.probe
			}

			rid := uuid.New()
			repo.seed(&domain.FleetResource{
				ID: rid, OwnerOrg: uuid.New(), ClaimKey: "c", Env: "prod",
				Tier: "dedicated", Phase: tt.stored, AppID: &appID,
			})

			st, err := uc.StatusOf(context.Background(), rid)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if st.Phase != string(tt.wantPhase) {
				t.Errorf("reported phase = %q, want %q", st.Phase, tt.wantPhase)
			}
			if len(st.Conditions) != len(tt.wantConditions) {
				t.Fatalf("conditions = %v, want %v", st.Conditions, tt.wantConditions)
			}
			for i, want := range tt.wantConditions {
				if st.Conditions[i] != want {
					t.Errorf("condition[%d] = %q, want %q", i, st.Conditions[i], want)
				}
			}
			stored, gerr := repo.GetResourceByHandle(context.Background(), rid)
			if gerr != nil {
				t.Fatalf("reload resource: %v", gerr)
			}
			if stored.Phase != tt.wantStored {
				t.Errorf("stored phase = %q, want %q", stored.Phase, tt.wantStored)
			}
		})
	}
}

func TestDecommissionDedicated_RejectsNoFinalSnapshot(t *testing.T) {
	repo := newFakeResourceRepo()
	snap := &fakeSnapshotter{}
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, snap, testEngine())

	rid := uuid.New()
	appID := uuid.New()
	repo.seed(&domain.FleetResource{ID: rid, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady, AppID: &appID})

	_, err := uc.DecommissionDedicated(context.Background(), rid, false)
	if !errors.Is(err, domain.ErrResourceFinalSnapshotRequired) {
		t.Fatalf("got %v, want ErrResourceFinalSnapshotRequired", err)
	}
	if snap.calls != 0 || len(prov.decommissioned) != 0 {
		t.Errorf("nothing must be torn down on reject")
	}
}

func TestDecommissionDedicated_SnapshotFirstThenTombstone(t *testing.T) {
	repo := newFakeResourceRepo()
	snap := &fakeSnapshotter{}
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, snap, testEngine())

	rid := uuid.New()
	appID := uuid.New()
	repo.seed(&domain.FleetResource{ID: rid, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady, AppID: &appID})

	final, err := uc.DecommissionDedicated(context.Background(), rid, true)
	if err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if snap.calls != 1 || snap.appID != appID || snap.resourceID != rid {
		t.Errorf("snapshot not taken snapshot-first: calls=%d", snap.calls)
	}
	// The teardown must hand back the recovery point it took: that is the only
	// thing that makes `final_snapshot=true` verifiable by the caller.
	if final == nil {
		t.Fatal("decommission must return the final recovery point")
	}
	if len(snap.produced) != 1 || final.ID != snap.produced[0].ID || final.ObjectKey != snap.produced[0].ObjectKey {
		t.Errorf("final recovery point = %+v, want the one the snapshotter created %+v", final, snap.produced)
	}
	if len(prov.decommissioned) != 1 || prov.decommissioned[0] != appID.String() {
		t.Errorf("app not decommissioned: %v", prov.decommissioned)
	}
	row, _ := repo.GetResourceByHandle(context.Background(), rid)
	if row.Phase != domain.FleetResourcePhaseDecommissioned || row.DecommissionedAt == nil {
		t.Errorf("row not tombstoned: phase=%q at=%v", row.Phase, row.DecommissionedAt)
	}
}

func TestDecommissionDedicated_SnapshotFailureAborts(t *testing.T) {
	repo := newFakeResourceRepo()
	snap := &fakeSnapshotter{err: errors.New("upload failed")}
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, snap, testEngine())

	rid := uuid.New()
	appID := uuid.New()
	repo.seed(&domain.FleetResource{ID: rid, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady, AppID: &appID})

	if _, err := uc.DecommissionDedicated(context.Background(), rid, true); err == nil {
		t.Fatalf("expected decommission to fail when snapshot fails")
	}
	if len(prov.decommissioned) != 0 {
		t.Errorf("app must not be torn down when snapshot fails")
	}
	row, _ := repo.GetResourceByHandle(context.Background(), rid)
	if row.Phase == domain.FleetResourcePhaseDecommissioned {
		t.Errorf("row must not be tombstoned when snapshot fails")
	}
}

// A snapshot CALL that succeeds is not a recovery point. An app with no volumes
// returns ([], nil) — success, nothing created — and tearing the resource down
// on that answer destroys it under a guarantee that never held.
func TestDecommissionDedicated_RefusesWhenNoRecoveryPointWasCreated(t *testing.T) {
	tests := []struct {
		name      string
		noVolumes bool
		wantErr   error
		wantTorn  bool
	}{
		{
			name:      "zero volumes yields zero recovery points and is refused",
			noVolumes: true,
			wantErr:   domain.ErrResourceFinalSnapshotRequired,
		},
		{
			name:     "one recovery point tears down",
			wantTorn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeResourceRepo()
			snap := &fakeSnapshotter{noVolumes: tt.noVolumes}
			prov := &fakeFleetProvisioner{}
			uc := NewFleetResourceProvisioner(prov, repo, nil, snap, testEngine())

			rid := uuid.New()
			appID := uuid.New()
			repo.seed(&domain.FleetResource{ID: rid, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady, AppID: &appID})

			final, err := uc.DecommissionDedicated(context.Background(), rid, true)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if torn := len(prov.decommissioned) == 1; torn != tt.wantTorn {
				t.Fatalf("app torn down = %v, want %v", torn, tt.wantTorn)
			}
			row, _ := repo.GetResourceByHandle(context.Background(), rid)
			if tombstoned := row.Phase == domain.FleetResourcePhaseDecommissioned; tombstoned != tt.wantTorn {
				t.Fatalf("tombstoned = %v, want %v", tombstoned, tt.wantTorn)
			}
			if tt.wantTorn && final == nil {
				t.Fatal("a successful snapshot-first teardown must report its recovery point")
			}
			if !tt.wantTorn && final != nil {
				t.Fatalf("a refused teardown must report no recovery point, got %+v", final)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// #p19-handed-token-not-rehandable — post-restart recovery, driven through the
// REAL orchestrator + the REAL in-memory token store. A fake provisioner could
// only pretend the token came back; these tests must observe the actual store.
// ─────────────────────────────────────────────────────────────────────

// orchProvisioner adapts the real *FleetOrchestrator to the FleetProvisioner
// port the resource control plane composes — the same three methods
// *FleetProvision routes to the orchestrator for the resident class.
type orchProvisioner struct{ orch *FleetOrchestrator }

func (p orchProvisioner) Provision(ctx context.Context, in FleetProvisionInput) (FleetProvisionOutput, error) {
	handle, url, err := p.orch.ProvisionApp(ctx, in)
	if err != nil {
		return FleetProvisionOutput{}, err
	}
	return FleetProvisionOutput{Handle: handle, URL: url}, nil
}

func (p orchProvisioner) Health(ctx context.Context, handle string) (FleetHealthOutput, error) {
	id, err := uuid.Parse(handle)
	if err != nil {
		return FleetHealthOutput{}, domain.ErrWorkloadNotFound
	}
	out, isApp, herr := p.orch.HealthApp(ctx, id)
	if herr != nil {
		return FleetHealthOutput{}, herr
	}
	if !isApp {
		return FleetHealthOutput{}, domain.ErrWorkloadNotFound
	}
	return out, nil
}

func (p orchProvisioner) Decommission(ctx context.Context, handle string) error {
	id, err := uuid.Parse(handle)
	if err != nil {
		return domain.ErrWorkloadNotFound
	}
	_, derr := p.orch.DecommissionApp(ctx, id)
	return derr
}

// handedTokenGateResolver reproduces the ONE property of the deployed
// HandedTokenEnvelopeResolver these tests depend on: with no handed token it
// fails closed with ErrNoHandedToken and never resolves anything. With a token
// it delegates to a REAL EnvelopeVaultResolver, because secret.SecretValue
// cannot be constructed with a value from outside the secret package.
type handedTokenGateResolver struct{ inner secret.Resolver }

func (r handedTokenGateResolver) Resolve(ctx context.Context, ref string, p secret.Principal) (secret.SecretValue, error) {
	if p.Token == "" {
		return secret.SecretValue{}, fmt.Errorf("%w: %s", secret.ErrNoHandedToken, ref)
	}
	return r.inner.Resolve(ctx, ref, p)
}

// resourceRecoveryHarness wires a resource provisioner over a real orchestrator,
// a real handed-token store, and a token-gated secret resolver.
type resourceRecoveryHarness struct {
	orch     orchHarness
	store    *FleetSecretTokenStore
	resource *FleetResourceProvisioner
	repo     *fakeResourceRepo
}

func newResourceRecoveryHarness(t *testing.T) resourceRecoveryHarness {
	t.Helper()
	h := newOrchHarness(oneLiveHost())
	h.orch.runtime.SetSecretResolver(handedTokenGateResolver{
		inner: secret.NewEnvelopeVaultResolver(stubKV{val: "vault:v1:ct"}, stubKEK{pt: []byte("s3cr3t")}),
	})
	store := NewFleetSecretTokenStore(context.Background(), nil, 0)
	h.orch.SetTokenStore(store)
	h.orch.runtime.SetTokenStore(store)

	repo := newFakeResourceRepo()
	return resourceRecoveryHarness{
		orch:     h,
		store:    store,
		repo:     repo,
		resource: NewFleetResourceProvisioner(orchProvisioner{h.orch}, repo, h.replicas, &fakeSnapshotter{}, testEngine()),
	}
}

// restart swaps in a FRESH token store on both seams — exactly what a
// runtime-service process restart does to a map that is memory-only by design,
// while every durable row survives.
func (h *resourceRecoveryHarness) restart() {
	h.store = NewFleetSecretTokenStore(context.Background(), nil, 0)
	h.orch.orch.SetTokenStore(h.store)
	h.orch.orch.runtime.SetTokenStore(h.store)
}

func dedicatedInputWithSecret(org uuid.UUID) ProvisionDedicatedInput {
	in := validDedicatedInput()
	in.OwnerOrg = org.String()
	in.SecretRefs = []string{secret.TenantRef(org, "prod/pg", "password")}
	return in
}

// TestProvisionDedicated_RecoversAfterRestart is the regression for
// #p19-handed-token-not-rehandable. It proves the whole failure mode end to end:
// after a restart the token store is empty, a reconcile alone can NEVER boot the
// engine again (every attempt dies with ErrNoHandedToken), and the declarative
// re-provision — the recovery a caller naturally attempts — puts the token back
// and gets the engine running.
func TestProvisionDedicated_RecoversAfterRestart(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	ctx := context.Background()
	org := uuid.New()
	h := newResourceRecoveryHarness(t)
	in := dedicatedInputWithSecret(org)

	first, err := h.resource.ProvisionDedicated(ctx, in)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	resourceID := uuid.MustParse(first.Handle)
	row, err := h.repo.GetResourceByHandle(ctx, resourceID)
	if err != nil || row.AppID == nil {
		t.Fatalf("resource row not persisted with an app: row=%+v err=%v", row, err)
	}
	appID := *row.AppID
	if _, ok := h.store.Get(appID); !ok {
		t.Fatalf("first provision must hand the token for app %s", appID)
	}
	if got := h.orch.replicas.countState(domain.ReplicaStateResident); got != 1 {
		t.Fatalf("resident replicas after first provision = %d, want 1", got)
	}

	// ── the restart ──────────────────────────────────────────────────────
	h.restart()
	if _, ok := h.store.Get(appID); ok {
		t.Fatalf("precondition: the token store must be EMPTY after a restart")
	}

	// The replica is gone with the host process; the reconciler sees it dead.
	replicas, err := h.orch.replicas.ListByApp(ctx, appID)
	if err != nil || len(replicas) != 1 {
		t.Fatalf("list replicas: %v (n=%d)", err, len(replicas))
	}
	preRestartReplica := replicas[0].ID
	dead := replicas[0]
	dead.State = domain.ReplicaStateDead
	if uerr := h.orch.replicas.Update(ctx, &dead); uerr != nil {
		t.Fatalf("mark replica dead: %v", uerr)
	}

	// The defect: reconcile alone can never bring it back — the boot fails closed
	// on the missing handed token, forever, with no path to re-supply one.
	if rerr := h.orch.orch.ReconcileApp(ctx, appID); rerr != nil {
		t.Fatalf("ReconcileApp: %v", rerr)
	}
	if got := h.orch.replicas.countState(domain.ReplicaStateResident); got != 0 {
		t.Fatalf("resident replicas after token-less reconcile = %d, want 0 (boot must fail closed)", got)
	}
	if got := h.orch.replicas.countState(domain.ReplicaStateDead); got == 0 {
		t.Fatalf("token-less boot should have left a dead replica")
	}

	// ── the recovery: an idempotent, same-revision re-provision ───────────
	second, err := h.resource.ProvisionDedicated(ctx, in)
	if err != nil {
		t.Fatalf("re-provision after restart: %v", err)
	}
	if second != first {
		t.Fatalf("response changed: %+v, want identical to %+v", second, first)
	}
	tok, ok := h.store.Get(appID)
	if !ok {
		t.Fatalf("re-provision must re-hand the token into the store")
	}
	if tok != in.VaultToken {
		t.Fatalf("re-handed token = %q, want the freshly supplied one", tok)
	}
	if got := h.orch.replicas.countState(domain.ReplicaStateResident); got != 1 {
		t.Fatalf("resident replicas after recovery = %d, want 1", got)
	}
	if got := h.orch.replicas.countState(domain.ReplicaStateDead); got != 0 {
		t.Fatalf("dead replicas after recovery = %d, want 0", got)
	}

	// The claim still points at the SAME app (no second app, no empty new volume).
	after, err := h.repo.GetResourceByHandle(ctx, resourceID)
	if err != nil {
		t.Fatalf("resource lookup: %v", err)
	}
	if after.AppID == nil || *after.AppID != appID {
		t.Fatalf("app_id = %v, want unchanged %v", after.AppID, appID)
	}
	apps, err := h.orch.apps.List(ctx)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps = %d, want exactly 1 (recovery must not mint a second app): %v", len(apps), err)
	}
	// The replacement is a NEW replica — the pre-restart one was dead and replaced,
	// which is what recovery means here.
	live, _ := h.orch.replicas.ListByApp(ctx, appID)
	if len(live) != 1 || live[0].ID == preRestartReplica {
		t.Fatalf("expected one fresh replica, got %+v", live)
	}
}

// TestProvisionDedicated_HealthyReProvisionDoesNotChurn pins the other half of
// the contract: a re-provision of a perfectly healthy resource re-hands a token
// the store already holds (harmless) and touches NOTHING else — the running VM
// is not restarted and the replica is not churned.
func TestProvisionDedicated_HealthyReProvisionDoesNotChurn(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	ctx := context.Background()
	org := uuid.New()
	h := newResourceRecoveryHarness(t)
	in := dedicatedInputWithSecret(org)

	first, err := h.resource.ProvisionDedicated(ctx, in)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	row, _ := h.repo.GetResourceByHandle(ctx, uuid.MustParse(first.Handle))
	appID := *row.AppID
	before, _ := h.orch.replicas.ListByApp(ctx, appID)
	if len(before) != 1 || before[0].State != domain.ReplicaStateResident {
		t.Fatalf("expected one resident replica, got %+v", before)
	}

	second, err := h.resource.ProvisionDedicated(ctx, in)
	if err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if second != first {
		t.Fatalf("response changed: %+v, want identical to %+v", second, first)
	}
	if tok, ok := h.store.Get(appID); !ok || tok != in.VaultToken {
		t.Fatalf("token store after re-hand = %q,%v", tok, ok)
	}
	after, _ := h.orch.replicas.ListByApp(ctx, appID)
	if len(after) != 1 {
		t.Fatalf("replicas after re-provision = %d, want 1", len(after))
	}
	if after[0].ID != before[0].ID {
		t.Fatalf("replica was churned: %s → %s", before[0].ID, after[0].ID)
	}
	if after[0].State != domain.ReplicaStateResident || after[0].PID == nil || before[0].PID == nil || *after[0].PID != *before[0].PID {
		t.Fatalf("running VM was restarted: before=%+v after=%+v", before[0], after[0])
	}
}

// ─────────────────────────────────────────────────────────────────────
// #two-orgs-same-claim-key-share-one-database. A dedicated resource's app was
// keyed 'resource/<claim_key>' with no org, and the ingress host is DERIVED from
// the component id (sanitizeSlug(component_id)-sanitizeSlug(env), unique-indexed
// by migrations/0006). Org-scoping the app row alone would therefore leave the
// second org's provision dying on a duplicate host: the org must live inside the
// component id too.
// ─────────────────────────────────────────────────────────────────────

func TestDedicatedDescriptor_ComponentIDIsOrgNamespaced(t *testing.T) {
	uc := NewFleetResourceProvisioner(&fakeFleetProvisioner{}, newFakeResourceRepo(), nil, &fakeSnapshotter{}, testEngine())

	in := validDedicatedInput()
	got := uc.dedicatedDescriptor(in).ComponentID
	if want := "resource/" + in.OwnerOrg + "/" + in.ClaimKey; got != want {
		t.Fatalf("component_id = %q, want %q", got, want)
	}

	// Two organisations, IDENTICAL claim key + env: the component ids must differ,
	// or both converge on one app row, one volume and one derived ingress host.
	a := validDedicatedInput()
	a.OwnerOrg = "11111111-1111-1111-1111-111111111111"
	b := validDedicatedInput()
	b.OwnerOrg = "22222222-2222-2222-2222-222222222222"
	if a.ClaimKey != b.ClaimKey || a.Env != b.Env {
		t.Fatalf("test setup: claims must be identical apart from the org")
	}
	idA := uc.dedicatedDescriptor(a).ComponentID
	idB := uc.dedicatedDescriptor(b).ComponentID
	if idA == idB {
		t.Fatalf("both orgs derive component_id %q — the cross-tenant defect", idA)
	}
}
