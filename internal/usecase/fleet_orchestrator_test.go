package usecase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// In-file fakes for the orchestrator. The orchestrator holds concrete
// *FleetScheduler + *FleetReplicaRuntime, so these fakes sit UNDER real
// instances of both (host lister + repos + materializer + booter).
// ─────────────────────────────────────────────────────────────────────

// orchReplicaRepo is a full stateful ReplicaRepository.
type orchReplicaRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.Replica
}

func newOrchReplicaRepo() *orchReplicaRepo {
	return &orchReplicaRepo{store: map[uuid.UUID]*domain.Replica{}}
}
func (f *orchReplicaRepo) Create(_ context.Context, r *domain.Replica) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.store[r.ID] = &cp
	return nil
}
func (f *orchReplicaRepo) Update(_ context.Context, r *domain.Replica) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.store[r.ID] = &cp
	return nil
}
func (f *orchReplicaRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.store[id]
	if !ok {
		return nil, domain.ErrReplicaNotFound
	}
	cp := *r
	return &cp, nil
}
func (f *orchReplicaRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Replica
	for _, r := range f.store {
		if r.AppID == appID {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *orchReplicaRepo) ListByHost(_ context.Context, hostID uuid.UUID) ([]domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Replica
	for _, r := range f.store {
		if r.HostID != nil && *r.HostID == hostID {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *orchReplicaRepo) ListByState(_ context.Context, state domain.ReplicaState) ([]domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Replica
	for _, r := range f.store {
		if r.State == state {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *orchReplicaRepo) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, id)
	return nil
}
func (f *orchReplicaRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.store)
}
func (f *orchReplicaRepo) countState(s domain.ReplicaState) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.store {
		if r.State == s {
			n++
		}
	}
	return n
}

// orchAppRepo is a stateful FleetAppRepository.
type orchAppRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID]*domain.FleetApp
}

func newOrchAppRepo(apps ...*domain.FleetApp) *orchAppRepo {
	r := &orchAppRepo{store: map[uuid.UUID]*domain.FleetApp{}}
	for _, a := range apps {
		cp := *a
		r.store[a.ID] = &cp
	}
	return r
}
func (f *orchAppRepo) Create(_ context.Context, a *domain.FleetApp) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.store[a.ID] = &cp
	return nil
}
func (f *orchAppRepo) Update(_ context.Context, a *domain.FleetApp) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *a
	f.store[a.ID] = &cp
	return nil
}
func (f *orchAppRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.store[id]
	if !ok {
		return nil, domain.ErrFleetAppNotFound
	}
	cp := *a
	return &cp, nil
}
func (f *orchAppRepo) FindByComponentEnv(_ context.Context, componentID, env string) (*domain.FleetApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.store {
		if a.ComponentID == componentID && a.Env == env {
			cp := *a
			return &cp, nil
		}
	}
	return nil, domain.ErrFleetAppNotFound
}
func (f *orchAppRepo) List(_ context.Context) ([]domain.FleetApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.FleetApp
	for _, a := range f.store {
		out = append(out, *a)
	}
	return out, nil
}
func (f *orchAppRepo) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, id)
	return nil
}
func (f *orchAppRepo) has(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.store[id]
	return ok
}

// orchHostLister returns a fixed live-host set for the scheduler.
type orchHostLister struct {
	hosts []domain.Host
}

func (f orchHostLister) ListLive(context.Context, time.Duration) ([]domain.Host, error) {
	return f.hosts, nil
}

// orchBooter returns a resident boot result per call. It never fails.
type orchBooter struct{ mu sync.Mutex }

func (b *orchBooter) BootTest(context.Context, ImageBootInput) (ImageTestResult, error) {
	return ImageTestResult{}, nil
}
func (b *orchBooter) BootResident(context.Context, ImageBootInput) (ImageResidentResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ImageResidentResult{PID: 4000, GuestIP: "", HostPort: 21000, NetIndex: 1, TapName: "t1", SocketPath: "/run/s1.sock"}, nil
}
func (b *orchBooter) Decommission(context.Context, ImageDecommissionInput) error { return nil }

// orchHarness bundles a wired orchestrator over fakes.
type orchHarness struct {
	orch     *FleetOrchestrator
	apps     *orchAppRepo
	replicas *orchReplicaRepo
}

func newOrchHarness(hosts []domain.Host, apps ...*domain.FleetApp) orchHarness {
	appRepo := newOrchAppRepo(apps...)
	repRepo := newOrchReplicaRepo()
	sched := NewFleetScheduler(orchHostLister{hosts: hosts}, repRepo, appRepo, time.Minute)
	runtime := NewFleetReplicaRuntime(fakeMaterializer{rootfs: "/work/r.ext4"}, &orchBooter{}, repRepo, appRepo, "/tmp/imgwork", "10.0.0.9")
	orch := NewFleetOrchestrator(appRepo, repRepo, sched, runtime)
	return orchHarness{orch: orch, apps: appRepo, replicas: repRepo}
}

func oneLiveHost() []domain.Host {
	return []domain.Host{{
		ID:             uuid.New(),
		Region:         "local",
		CapacityVCPU:   100,
		CapacityMemMB:  100000,
		CapacityDiskMB: 100000,
		Health:         domain.HostHealthHealthy,
		Status:         domain.HostStatusActive,
	}}
}

func testFleetApp(desired int) *domain.FleetApp {
	now := time.Now().UTC()
	return &domain.FleetApp{
		ID:              uuid.New(),
		ComponentID:     "comp-1",
		Env:             "prod",
		ImageRepository: "org/app",
		ImageDigest:     "sha256:abc",
		DesiredReplicas: desired,
		Port:            8080,
		ResourcesVCPU:   2,
		ResourcesMemMB:  1024,
		RestartPolicy:   domain.RestartPolicyAlways,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func TestReconcileApp(t *testing.T) {
	// processAlive true so refreshed resident replicas stay healthy (guest IP is
	// left empty so RefreshHealth never dials a fake endpoint).
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	tests := []struct {
		name         string
		desired      int
		seed         func(appID uuid.UUID) []*domain.Replica
		hosts        []domain.Host
		wantErr      bool
		wantTotal    int
		wantResident int
	}{
		{
			name:         "shortfall boots N replicas",
			desired:      3,
			hosts:        oneLiveHost(),
			wantTotal:    3,
			wantResident: 3,
		},
		{
			name:    "dead replica decommissioned and replaced",
			desired: 1,
			seed: func(appID uuid.UUID) []*domain.Replica {
				return []*domain.Replica{{
					ID: uuid.New(), AppID: appID, State: domain.ReplicaStateDead,
					RestartPolicy: domain.RestartPolicyAlways, CreatedAt: time.Now().UTC(),
				}}
			},
			hosts:        oneLiveHost(),
			wantTotal:    1,
			wantResident: 1,
		},
		{
			name:    "surplus decommissioned",
			desired: 1,
			seed: func(appID uuid.UUID) []*domain.Replica {
				pid := 4000
				reps := make([]*domain.Replica, 3)
				for i := range reps {
					reps[i] = &domain.Replica{
						ID: uuid.New(), AppID: appID, State: domain.ReplicaStateResident,
						PID: &pid, RestartPolicy: domain.RestartPolicyAlways,
						CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
					}
				}
				return reps
			},
			hosts:        oneLiveHost(),
			wantTotal:    1,
			wantResident: 1,
		},
		{
			name:         "no schedulable host is non-fatal",
			desired:      1,
			hosts:        nil, // empty live set → ErrNoSchedulableHost
			wantErr:      false,
			wantTotal:    0,
			wantResident: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := testFleetApp(tt.desired)
			h := newOrchHarness(tt.hosts, app)
			if tt.seed != nil {
				for _, r := range tt.seed(app.ID) {
					if err := h.replicas.Create(context.Background(), r); err != nil {
						t.Fatalf("seed replica: %v", err)
					}
				}
			}

			err := h.orch.ReconcileApp(context.Background(), app.ID)
			if tt.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ReconcileApp: %v", err)
			}
			if got := h.replicas.count(); got != tt.wantTotal {
				t.Fatalf("total replicas = %d, want %d", got, tt.wantTotal)
			}
			if got := h.replicas.countState(domain.ReplicaStateResident); got != tt.wantResident {
				t.Fatalf("resident replicas = %d, want %d", got, tt.wantResident)
			}
		})
	}
}

func TestOrchestrator_UnknownAppReturnsFalse(t *testing.T) {
	h := newOrchHarness(oneLiveHost())
	unknown := uuid.New()

	if _, isApp, err := h.orch.HealthApp(context.Background(), unknown); err != nil || isApp {
		t.Fatalf("HealthApp unknown: isApp=%v err=%v, want false,nil", isApp, err)
	}
	if isApp, err := h.orch.DecommissionApp(context.Background(), unknown); err != nil || isApp {
		t.Fatalf("DecommissionApp unknown: isApp=%v err=%v, want false,nil", isApp, err)
	}
	if isApp, err := h.orch.ScaleApp(context.Background(), unknown, 3); err != nil || isApp {
		t.Fatalf("ScaleApp unknown: isApp=%v err=%v, want false,nil", isApp, err)
	}
}

func TestOrchestrator_ProvisionNilSecretRefsPersistsNonNil(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	// No SecretRefs — the every-secret-less-provision path that wrote SQL NULL into
	// the JSONB NOT NULL secret_refs column before the nil→[] normalization.
	in := FleetProvisionInput{
		ComponentID: "comp-1", Env: "prod",
		Registry: "reg", Repository: "org/app", Digest: "sha256:abc",
		VCPU: 2, MemoryMB: 1024, Port: 8080,
	}
	handle, _, err := h.orch.ProvisionApp(context.Background(), in)
	if err != nil {
		t.Fatalf("ProvisionApp: %v", err)
	}
	appID, err := uuid.Parse(handle)
	if err != nil {
		t.Fatalf("handle is not a uuid: %v", err)
	}

	// Create-site: persisted SecretRefs must be non-nil so GORM writes [] not NULL.
	app, err := h.apps.FindByID(context.Background(), appID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if app.SecretRefs == nil {
		t.Fatalf("SecretRefs persisted nil on create; want non-nil empty slice")
	}
	if len(app.SecretRefs) != 0 {
		t.Fatalf("SecretRefs = %v, want empty", app.SecretRefs)
	}

	// Update-site: re-provision the same (component, env) with nil refs must also
	// keep SecretRefs non-nil.
	if _, _, err := h.orch.ProvisionApp(context.Background(), in); err != nil {
		t.Fatalf("re-ProvisionApp: %v", err)
	}
	app, err = h.apps.FindByID(context.Background(), appID)
	if err != nil {
		t.Fatalf("FindByID after update: %v", err)
	}
	if app.SecretRefs == nil {
		t.Fatalf("SecretRefs persisted nil on update; want non-nil empty slice")
	}
}

func TestOrchestrator_ProvisionThenScaleThenDecommission(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	in := FleetProvisionInput{
		ComponentID: "comp-1", Env: "prod",
		Registry: "reg", Repository: "org/app", Digest: "sha256:abc",
		VCPU: 2, MemoryMB: 1024, Port: 8080,
	}
	handle, _, err := h.orch.ProvisionApp(context.Background(), in)
	if err != nil {
		t.Fatalf("ProvisionApp: %v", err)
	}
	appID, err := uuid.Parse(handle)
	if err != nil {
		t.Fatalf("handle is not a uuid: %v", err)
	}
	if !h.apps.has(appID) {
		t.Fatalf("app not persisted")
	}
	if got := h.replicas.countState(domain.ReplicaStateResident); got != 1 {
		t.Fatalf("resident after provision = %d, want 1", got)
	}

	if isApp, err := h.orch.ScaleApp(context.Background(), appID, 3); err != nil || !isApp {
		t.Fatalf("ScaleApp: isApp=%v err=%v", isApp, err)
	}
	if got := h.replicas.countState(domain.ReplicaStateResident); got != 3 {
		t.Fatalf("resident after scale = %d, want 3", got)
	}

	if isApp, err := h.orch.DecommissionApp(context.Background(), appID); err != nil || !isApp {
		t.Fatalf("DecommissionApp: isApp=%v err=%v", isApp, err)
	}
	if h.apps.has(appID) {
		t.Fatalf("app row should be deleted")
	}
	if got := h.replicas.count(); got != 0 {
		t.Fatalf("replicas after decommission = %d, want 0", got)
	}
}
