package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
}

func (f *fakeSnapshotter) SnapshotAppVolumes(_ context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	f.calls++
	f.resourceID = resourceID
	f.appID = appID
	if f.err != nil {
		return nil, f.err
	}
	return []domain.FleetResourceRecoveryPoint{{ID: uuid.New(), ResourceID: resourceID}}, nil
}

// fakeResourceReplicaRepo serves FindByID + ListByApp from in-memory maps
// (distinct from the scheduler test's fakeReplicaRepo).
type fakeResourceReplicaRepo struct {
	byApp map[uuid.UUID][]domain.Replica
	byID  map[uuid.UUID]*domain.Replica
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
	if gi.ComponentID != "resource/orders-db" {
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

func TestProvisionDedicated_IdempotentSameRevision(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())

	in := validDedicatedInput()
	owner := uuid.MustParse(in.OwnerOrg)
	existingID := uuid.New()
	repo.seed(&domain.FleetResource{
		ID: existingID, OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
		Revision: 1, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady,
	})

	out, err := uc.ProvisionDedicated(context.Background(), in)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if out.Handle != existingID.String() {
		t.Errorf("handle = %q, want existing %q", out.Handle, existingID)
	}
	if prov.provisionCalls != 0 {
		t.Errorf("declarative ensure must not re-provision (calls=%d)", prov.provisionCalls)
	}
}

func TestProvisionDedicated_ConvergeRejected(t *testing.T) {
	repo := newFakeResourceRepo()
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, &fakeSnapshotter{}, testEngine())

	in := validDedicatedInput()
	in.Revision = 2
	owner := uuid.MustParse(in.OwnerOrg)
	repo.seed(&domain.FleetResource{
		ID: uuid.New(), OwnerOrg: owner, ClaimKey: in.ClaimKey, Env: in.Env,
		Revision: 1, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady,
	})

	_, err := uc.ProvisionDedicated(context.Background(), in)
	if !errors.Is(err, domain.ErrResourceConvergeNotSupported) {
		t.Fatalf("got %v, want ErrResourceConvergeNotSupported", err)
	}
	if prov.provisionCalls != 0 {
		t.Errorf("converge reject must not provision (calls=%d)", prov.provisionCalls)
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
	guestReplica := domain.Replica{ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident, GuestIP: "10.0.0.9"}
	replicas.byApp[appID] = []domain.Replica{guestReplica}
	uc := NewFleetResourceProvisioner(prov, repo, replicas, &fakeSnapshotter{}, testEngine())

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

func TestDecommissionDedicated_RejectsNoFinalSnapshot(t *testing.T) {
	repo := newFakeResourceRepo()
	snap := &fakeSnapshotter{}
	prov := &fakeFleetProvisioner{}
	uc := NewFleetResourceProvisioner(prov, repo, nil, snap, testEngine())

	rid := uuid.New()
	appID := uuid.New()
	repo.seed(&domain.FleetResource{ID: rid, Tier: "dedicated", Phase: domain.FleetResourcePhaseReady, AppID: &appID})

	err := uc.DecommissionDedicated(context.Background(), rid, false)
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

	if err := uc.DecommissionDedicated(context.Background(), rid, true); err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if snap.calls != 1 || snap.appID != appID || snap.resourceID != rid {
		t.Errorf("snapshot not taken snapshot-first: calls=%d", snap.calls)
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

	if err := uc.DecommissionDedicated(context.Background(), rid, true); err == nil {
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
