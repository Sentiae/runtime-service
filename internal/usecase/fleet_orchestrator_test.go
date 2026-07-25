package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
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
func (f *orchAppRepo) FindByComponentEnv(_ context.Context, componentID, env, ownerOrg string) (*domain.FleetApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.store {
		// Keyed on (component, env, owner) exactly like the unique index in
		// migrations/0014. A fake that matched on (component, env) alone would keep
		// passing while the real cross-tenant defect was reintroduced.
		if a.ComponentID == componentID && a.Env == env && a.OwnerOrg == ownerOrg {
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

// orchRouteRepo is a stateful RouteRepository (rt#8).
type orchRouteRepo struct {
	mu    sync.Mutex
	store map[uuid.UUID][]domain.Route
}

func newOrchRouteRepo() *orchRouteRepo {
	return &orchRouteRepo{store: map[uuid.UUID][]domain.Route{}}
}
func (f *orchRouteRepo) Create(_ context.Context, r *domain.Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Enforce host uniqueness the way the real unique index does (migrations/0006),
	// and fail with the same translated sentinel the real repository returns. A fake
	// that accepted duplicate hosts could not fail for the right reason: two orgs
	// colliding on one derived host would look like success here and break only in
	// production.
	for _, routes := range f.store {
		for i := range routes {
			if routes[i].HostPattern == r.HostPattern {
				return fmt.Errorf("%w: %s", domain.ErrIngressHostTaken, r.HostPattern)
			}
		}
	}
	f.store[r.AppID] = append(f.store[r.AppID], *r)
	return nil
}
func (f *orchRouteRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Route, len(f.store[appID]))
	copy(out, f.store[appID])
	return out, nil
}
func (f *orchRouteRepo) DeleteByApp(_ context.Context, appID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, appID)
	return nil
}
func (f *orchRouteRepo) FindByHost(_ context.Context, host string) (*domain.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, routes := range f.store {
		for i := range routes {
			if routes[i].HostPattern == host || routes[i].CustomDomain == host {
				cp := routes[i]
				return &cp, nil
			}
		}
	}
	return nil, domain.ErrRouteNotFound
}

// fakeIngressSyncer records the last route set pushed.
type fakeIngressSyncer struct {
	mu    sync.Mutex
	last  []IngressRoute
	calls int
}

func (f *fakeIngressSyncer) Sync(_ context.Context, routes []IngressRoute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = routes
	return nil
}
func (f *fakeIngressSyncer) snapshot() (int, []IngressRoute) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
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

// Two distinct owning organisations. Real uuids because owner_org carries the
// attested tenant all the way into secret resolution.
const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"
)

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

// ─────────────────────────────────────────────────────────────────────
// The backing-file placement precondition. A volume-bearing app whose data file
// is gone cannot boot, and re-placing it every tick burned a replica row, an
// image materialize and a secret resolve each time, forever.
// ─────────────────────────────────────────────────────────────────────

// newStatefulOrchHarness wires a volume-bearing app whose single volume points
// at `backing` (which the caller creates or removes at will), through the REAL
// FleetVolumeManager the reconciler consults in production.
func newStatefulOrchHarness(t *testing.T, backing string) (orchHarness, *domain.FleetApp) {
	t.Helper()
	app := testFleetApp(1)
	h := newOrchHarness(oneLiveHost(), app)
	vol := volWithBacking(app.ID, backing)
	h.orch.SetVolumeManager(NewFleetVolumeManager(newVolRepoFake(vol), &recordingBackend{}, filepath.Dir(backing)))
	return h, app
}

func TestReconcileApp_BackingFilePrecondition(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	tests := []struct {
		name string
		// setup prepares the on-disk state and returns the volume's backing path.
		setup     func(t *testing.T) string
		wantPlace bool
	}{
		{
			name: "backing file present places normally",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "vol.ext4")
				if err := os.WriteFile(p, []byte("DATA"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantPlace: true,
		},
		{
			// File gone, store mounted: the data-loss shape. No replica, and — the
			// point of the gate — no image materialize and no secret resolve either.
			name: "backing file missing from a mounted store places nothing",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "vol.ext4")
			},
			wantPlace: false,
		},
		{
			// The whole directory is gone: an UNMOUNTED volume store looks exactly
			// like this and the data is intact. Deferring is the only honest move —
			// concluding data loss would send an operator chasing a restore for data
			// that never left.
			name: "missing volume store defers, it does not conclude data loss",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "not-mounted", "vol.ext4")
			},
			wantPlace: false,
		},
		{
			// An app with no volume at all is untouched by any of this.
			name:      "stateless app is unaffected",
			setup:     func(*testing.T) string { return "" },
			wantPlace: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := tt.setup(t)
			var h orchHarness
			var app *domain.FleetApp
			if backing == "" {
				app = testFleetApp(1)
				h = newOrchHarness(oneLiveHost(), app)
			} else {
				h, app = newStatefulOrchHarness(t, backing)
			}

			if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
				t.Fatalf("ReconcileApp: %v", err)
			}
			if placed := h.replicas.count() > 0; placed != tt.wantPlace {
				t.Fatalf("replica placed = %v, want %v", placed, tt.wantPlace)
			}
		})
	}
}

// ⚠ THE ASSERTION THAT MATTERS: the gate is a per-tick PRECONDITION, not a
// latch. Nothing in this service clears a degraded volume status, so latching
// would trade a churning resource for a permanently stuck one. The moment the
// backing file returns — an operator remounts, a restore lands it — the very
// next tick must place normally, with no operator verb and no state to unwind.
func TestReconcileApp_BackingFilePreconditionIsNotALatch(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	backing := filepath.Join(t.TempDir(), "vol.ext4")
	h, app := newStatefulOrchHarness(t, backing)

	// Tick 1 + 2, file absent: nothing placed, and repeated ticks do not
	// accumulate anything either.
	for i := 1; i <= 2; i++ {
		if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if got := h.replicas.count(); got != 0 {
			t.Fatalf("tick %d placed %d replicas, want 0", i, got)
		}
	}

	// The file comes back. The NEXT tick must recover on its own.
	if err := os.WriteFile(backing, []byte("DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.ReconcileApp(context.Background(), app.ID); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	if got := h.replicas.count(); got != 1 {
		t.Fatalf("replicas after the file returned = %d, want 1 (the gate must not latch)", got)
	}
	if got := h.replicas.countState(domain.ReplicaStateResident); got != 1 {
		t.Fatalf("resident replicas = %d, want 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Condition-log throttling. The precondition is re-evaluated every ~10s tick
// forever, so an unconvergeable resource used to emit its blocked line every
// tick and bury every other line in the journal. The CHECK still runs each
// tick — only the line is throttled.
// ─────────────────────────────────────────────────────────────────────

// condCountHandler counts emitted records by their "condition" attribute.
type condCountHandler struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCondCountHandler() *condCountHandler {
	return &condCountHandler{counts: map[string]int{}}
}
func (h *condCountHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *condCountHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "condition" {
			h.counts[a.Value.String()]++
		}
		return true
	})
	return nil
}
func (h *condCountHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *condCountHandler) WithGroup(string) slog.Handler      { return h }
func (h *condCountHandler) count(condition string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[condition]
}

func TestPlaceableOnBackingFile_ConditionLogThrottle(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		// backingPath returns the volume's backing path under root.
		backingPath func(root string) string
		// breakIt makes the condition true; heal makes the app placeable again.
		breakIt func(t *testing.T, backing string)
		heal    func(t *testing.T, backing string)
	}{
		{
			name:        "backing file missing from a mounted store",
			condition:   conditionBackingFileMissing,
			backingPath: func(root string) string { return filepath.Join(root, "vol.ext4") },
			breakIt: func(t *testing.T, backing string) {
				if err := os.RemoveAll(backing); err != nil {
					t.Fatal(err)
				}
			},
			heal: func(t *testing.T, backing string) {
				if err := os.WriteFile(backing, []byte("DATA"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "volume store unavailable",
			condition:   conditionVolumeStoreUnavailable,
			backingPath: func(root string) string { return filepath.Join(root, "not-mounted", "vol.ext4") },
			breakIt: func(t *testing.T, backing string) {
				if err := os.RemoveAll(filepath.Dir(backing)); err != nil {
					t.Fatal(err)
				}
			},
			heal: func(t *testing.T, backing string) {
				if err := os.MkdirAll(filepath.Dir(backing), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(backing, []byte("DATA"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backing := tt.backingPath(t.TempDir())
			h, app := newStatefulOrchHarness(t, backing)

			clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
			h.orch.clock = func() time.Time { return clock }

			sink := newCondCountHandler()
			ctx := logger.NewContext(context.Background(), slog.New(sink))

			tick := func(wantPlaceable bool, wantLines int, step string) {
				t.Helper()
				if got := h.orch.placeableOnBackingFile(ctx, app.ID); got != wantPlaceable {
					t.Fatalf("%s: placeable = %v, want %v (throttling must not change placement)", step, got, wantPlaceable)
				}
				if got := sink.count(tt.condition); got != wantLines {
					t.Fatalf("%s: %s lines = %d, want %d", step, tt.condition, got, wantLines)
				}
			}

			tt.breakIt(t, backing)
			tick(false, 1, "first occurrence logs immediately")
			tick(false, 1, "second tick inside the window is suppressed")

			clock = clock.Add(conditionLogInterval - time.Second)
			tick(false, 1, "still inside the window")

			clock = clock.Add(2 * time.Second)
			tick(false, 2, "window elapsed logs again")

			// The condition clears, then recurs: the recurrence must log on its
			// first tick rather than wait out a stale timestamp.
			tt.heal(t, backing)
			tick(true, 2, "healed places and logs nothing")
			tt.breakIt(t, backing)
			tick(false, 3, "recurrence logs immediately")
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
		ComponentID: "comp-1", Env: "prod", OwnerOrg: orgA,
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

func TestOrchestrator_IngressRouteAndSync(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	routes := newOrchRouteRepo()
	syncer := &fakeIngressSyncer{}
	h.orch.SetIngress(routes, "fleet.sentiae.local", syncer)

	in := FleetProvisionInput{
		ComponentID: "urlshortener", Env: "prod", OwnerOrg: orgA,
		Registry: "reg", Repository: "org/app", Digest: "sha256:abc",
		VCPU: 2, MemoryMB: 1024, Port: 8080,
	}
	handle, url, err := h.orch.ProvisionApp(context.Background(), in)
	if err != nil {
		t.Fatalf("ProvisionApp: %v", err)
	}
	appID, err := uuid.Parse(handle)
	if err != nil {
		t.Fatalf("handle is not a uuid: %v", err)
	}

	// With ingress wired the URL is the stable Caddy host, not a replica endpoint.
	const wantHost = "urlshortener-prod.fleet.sentiae.local"
	if url != "https://"+wantHost {
		t.Fatalf("ProvisionApp url = %q, want https://%s", url, wantHost)
	}

	// A route was recorded for the app.
	rs, _ := routes.ListByApp(context.Background(), appID)
	if len(rs) != 1 || rs[0].HostPattern != wantHost || rs[0].PathPrefix != "/" {
		t.Fatalf("routes = %+v, want one host=%s path=/", rs, wantHost)
	}

	// Re-provision is idempotent: still exactly one route.
	if _, _, err := h.orch.ProvisionApp(context.Background(), in); err != nil {
		t.Fatalf("re-ProvisionApp: %v", err)
	}
	if rs, _ := routes.ListByApp(context.Background(), appID); len(rs) != 1 {
		t.Fatalf("routes after re-provision = %d, want 1 (idempotent)", len(rs))
	}

	// SyncIngress pushes the route with the resident replica as an upstream.
	if err := h.orch.SyncIngress(context.Background()); err != nil {
		t.Fatalf("SyncIngress: %v", err)
	}
	calls, last := syncer.snapshot()
	if calls != 1 {
		t.Fatalf("syncer calls = %d, want 1", calls)
	}
	if len(last) != 1 {
		t.Fatalf("synced routes = %d, want 1", len(last))
	}
	if last[0].Host != wantHost {
		t.Fatalf("synced host = %q, want %q", last[0].Host, wantHost)
	}
	if len(last[0].Upstreams) != 1 {
		t.Fatalf("synced upstreams = %v, want 1 resident", last[0].Upstreams)
	}

	// Decommission drops the routes.
	if _, err := h.orch.DecommissionApp(context.Background(), appID); err != nil {
		t.Fatalf("DecommissionApp: %v", err)
	}
	if rs, _ := routes.ListByApp(context.Background(), appID); len(rs) != 0 {
		t.Fatalf("routes after decommission = %d, want 0", len(rs))
	}
}

func TestOrchestrator_ProvisionThenScaleThenDecommission(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	in := FleetProvisionInput{
		ComponentID: "comp-1", Env: "prod", OwnerOrg: orgA,
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

// ListBySystemEnv satisfies FleetAppRepository. This fake predates P21 network
// membership and models none, so it matches nothing.
func (f *orchAppRepo) ListBySystemEnv(context.Context, string, string, string) ([]domain.FleetApp, error) {
	return nil, nil
}

// ─────────────────────────────────────────────────────────────────────
// #two-orgs-same-claim-key-share-one-database. fleet_apps was unique on
// (component_id, env) with no org, and the upsert looked apps up the same way,
// so two organisations naming the same component+env CONVERGED ON ONE app row —
// one VM, one volume, one Postgres, and one owner_org used to scope BOTH orgs'
// secret resolution. Both must now get their own app.
// ─────────────────────────────────────────────────────────────────────

func TestProvisionApp_SameComponentEnvDifferentOrgs_GetsSeparateApps(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	vols := newVolRepoFake()
	h.orch.SetVolumeManager(NewFleetVolumeManager(vols, &recordingBackend{}, "/vol"))

	provision := func(org string) uuid.UUID {
		t.Helper()
		handle, _, err := h.orch.ProvisionApp(context.Background(), FleetProvisionInput{
			ComponentID: "shared-comp", Env: "prod", OwnerOrg: org,
			Registry: "reg", Repository: "org/app", Digest: "sha256:abc",
			VCPU: 2, MemoryMB: 1024, Port: 8080,
			Volumes: []VolumeSpecInput{{SizeMB: 1024, MountPath: "/data"}},
		})
		if err != nil {
			t.Fatalf("ProvisionApp(%s): %v", org, err)
		}
		id, perr := uuid.Parse(handle)
		if perr != nil {
			t.Fatalf("handle is not a uuid: %v", perr)
		}
		return id
	}

	appA := provision(orgA)
	appB := provision(orgB)

	// Two app rows, not one re-owned row.
	if appA == appB {
		t.Fatalf("both orgs converged on one app row %s — the cross-tenant defect", appA)
	}
	rowA, err := h.apps.FindByID(context.Background(), appA)
	if err != nil {
		t.Fatalf("load app A: %v", err)
	}
	rowB, err := h.apps.FindByID(context.Background(), appB)
	if err != nil {
		t.Fatalf("load app B: %v", err)
	}
	if rowA.OwnerOrg != orgA || rowB.OwnerOrg != orgB {
		t.Fatalf("owner orgs = %q / %q, want %q / %q", rowA.OwnerOrg, rowB.OwnerOrg, orgA, orgB)
	}

	// Two volume sets. One shared volume would be one org's data mounted into the
	// other org's VM.
	volsA, err := vols.ListByApp(context.Background(), appA)
	if err != nil {
		t.Fatalf("list volumes A: %v", err)
	}
	volsB, err := vols.ListByApp(context.Background(), appB)
	if err != nil {
		t.Fatalf("list volumes B: %v", err)
	}
	if len(volsA) != 1 || len(volsB) != 1 {
		t.Fatalf("volumes = %d / %d, want 1 each", len(volsA), len(volsB))
	}
	if volsA[0].ID == volsB[0].ID {
		t.Fatalf("both orgs share volume %s — one org's data in the other's VM", volsA[0].ID)
	}

	// And the first org's row was not mutated into the second's on the way.
	if reloaded, rerr := h.apps.FindByID(context.Background(), appA); rerr != nil || reloaded.OwnerOrg != orgA {
		t.Fatalf("app A after B's provision: owner=%v err=%v, want %s", reloaded, rerr, orgA)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The ingress-wired sibling of the test above. The org-separation test runs with
// uc.routes nil, so ensureRoute returns early and the ROUTE path — where the
// derived host is the shared key — is never exercised, while production wires
// ingress unconditionally (internal/di/container.go). Two orgs claiming the same
// resource claim key must get two distinct hosts and two route rows.
// ─────────────────────────────────────────────────────────────────────

func TestProvisionApp_SameResourceClaimDifferentOrgs_GetsSeparateIngressHosts(t *testing.T) {
	origAlive := processAlive
	processAlive = func(int) bool { return true }
	defer func() { processAlive = origAlive }()

	h := newOrchHarness(oneLiveHost())
	routes := newOrchRouteRepo()
	h.orch.SetIngress(routes, "fleet.sentiae.local", &fakeIngressSyncer{})

	// The component id a dedicated resource derives (fleet_resource_provision.go
	// dedicatedDescriptor): org-namespaced claim key. "postgres-main" is the
	// plausible claim key that overflows the DNS label once the 36-char org uuid is
	// inside the id — so this also covers the truncation path end to end.
	const claim = "postgres-main"
	provision := func(org string) (uuid.UUID, string) {
		t.Helper()
		handle, url, err := h.orch.ProvisionApp(context.Background(), FleetProvisionInput{
			ComponentID: "resource/" + org + "/" + claim, Env: "prod", OwnerOrg: org,
			Registry: "reg", Repository: "org/pg", Digest: "sha256:abc",
			VCPU: 2, MemoryMB: 1024, Port: 5432,
		})
		if err != nil {
			t.Fatalf("ProvisionApp(%s): %v", org, err)
		}
		id, perr := uuid.Parse(handle)
		if perr != nil {
			t.Fatalf("handle is not a uuid: %v", perr)
		}
		return id, url
	}

	appA, urlA := provision(orgA)
	appB, urlB := provision(orgB)

	if urlA == urlB {
		t.Fatalf("both orgs got ingress url %q — one org's traffic reaches the other's database", urlA)
	}

	rsA, err := routes.ListByApp(context.Background(), appA)
	if err != nil {
		t.Fatalf("list routes A: %v", err)
	}
	rsB, err := routes.ListByApp(context.Background(), appB)
	if err != nil {
		t.Fatalf("list routes B: %v", err)
	}
	if len(rsA) != 1 || len(rsB) != 1 {
		t.Fatalf("routes = %d / %d, want 1 each", len(rsA), len(rsB))
	}
	if rsA[0].HostPattern == rsB[0].HostPattern {
		t.Fatalf("both orgs routed host %q — the cross-tenant defect", rsA[0].HostPattern)
	}
	// Each url is the app's own route host, and each host is a valid DNS name.
	for _, c := range []struct {
		org, url string
		route    domain.Route
	}{{orgA, urlA, rsA[0]}, {orgB, urlB, rsB[0]}} {
		if c.url != "https://"+c.route.HostPattern {
			t.Fatalf("%s: url = %q, want https://%s", c.org, c.url, c.route.HostPattern)
		}
		label := strings.SplitN(c.route.HostPattern, ".", 2)[0]
		if len(label) > dnsLabelMaxLen {
			t.Fatalf("%s: first label %q is %d octets, over the %d-octet DNS limit", c.org, label, len(label), dnsLabelMaxLen)
		}
	}
}

// TestHostForApp covers the DNS-label boundary. The first label is
// sanitizeSlug(component_id)-sanitizeSlug(env) and component_id is unvalidated
// free text, so an over-long label must be folded deterministically — an invalid
// label is a host no resolver accepts and no CA will certify.
func TestHostForApp(t *testing.T) {
	const domainSuffix = "fleet.sentiae.local"
	uc := &FleetOrchestrator{ingressDomain: domainSuffix}

	// wantFolded is the expected fold of an over-long label, recomputed here from
	// the SPEC (prefix of the full label + '-' + 8 hex of sha256(full label)) rather
	// than by calling the code under test.
	wantFolded := func(fullLabel string) string {
		sum := sha256.Sum256([]byte(fullLabel))
		prefix := strings.TrimRight(fullLabel[:dnsLabelMaxLen-1-hostLabelHashLen], "-")
		return prefix + "-" + hex.EncodeToString(sum[:])[:hostLabelHashLen] + "." + domainSuffix
	}

	// Exactly 63: 58 + len("-prod") = 63, the last length that must pass through.
	exactly63 := strings.Repeat("a", 58)
	// 64: one octet over — the case a plausible claim key produces.
	sixtyFour := strings.Repeat("b", 59)
	longClaim := "resource/" + orgA + "/postgres-main-primary-eu-central"
	longUUID := "resource/" + strings.Repeat("f", 80) + "/pg"
	// Same first 54 slug octets, differing only past the truncation point: without
	// the hash these two would collapse onto ONE host.
	twinA := strings.Repeat("c", 54) + "-alpha"
	twinB := strings.Repeat("c", 54) + "-omega"

	tests := []struct {
		name        string
		componentID string
		env         string
		want        string
	}{
		{
			// ⚠ Byte-identical to what this has always produced. Changing an existing
			// host would silently orphan the live route pointing at the old one.
			name:        "short id is unchanged",
			componentID: "urlshortener",
			env:         "prod",
			want:        "urlshortener-prod." + domainSuffix,
		},
		{
			name:        "label of exactly 63 octets passes through",
			componentID: exactly63,
			env:         "prod",
			want:        exactly63 + "-prod." + domainSuffix,
		},
		{
			name:        "label of 64 octets is folded",
			componentID: sixtyFour,
			env:         "prod",
			want:        wantFolded(sixtyFour + "-prod"),
		},
		{
			name:        "long resource claim key is folded",
			componentID: longClaim,
			env:         "prod",
			want:        wantFolded(sanitizeSlug(longClaim) + "-prod"),
		},
		{
			name:        "over-long component uuid is folded",
			componentID: longUUID,
			env:         "staging",
			want:        wantFolded(sanitizeSlug(longUUID) + "-staging"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &domain.FleetApp{ComponentID: tt.componentID, Env: tt.env}
			got := uc.hostForApp(app)
			if got != tt.want {
				t.Fatalf("hostForApp = %q, want %q", got, tt.want)
			}
			label := strings.SplitN(got, ".", 2)[0]
			if len(label) > dnsLabelMaxLen {
				t.Fatalf("first label %q is %d octets, over the %d-octet DNS limit", label, len(label), dnsLabelMaxLen)
			}
			if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				t.Fatalf("first label %q begins or ends with '-' — not a legal DNS label", label)
			}
			// ensureRoute re-derives the host (migration 0015 deletes resource routes so
			// they regenerate), so the derivation must be stable across calls.
			if again := uc.hostForApp(app); again != got {
				t.Fatalf("hostForApp is not deterministic: %q then %q", got, again)
			}
		})
	}

	// Truncation must not merge two distinct ids.
	a := uc.hostForApp(&domain.FleetApp{ComponentID: twinA, Env: "prod"})
	b := uc.hostForApp(&domain.FleetApp{ComponentID: twinB, Env: "prod"})
	if a == b {
		t.Fatalf("two ids differing past the truncation point collapsed onto host %q", a)
	}

	// A cut landing on the separator must not leave a trailing '-'.
	cutOnDash := uc.hostForApp(&domain.FleetApp{
		ComponentID: strings.Repeat("d", 53) + "/" + strings.Repeat("e", 40), Env: "prod",
	})
	if label := strings.SplitN(cutOnDash, ".", 2)[0]; strings.Contains(label, "--") || strings.HasSuffix(label, "-") {
		t.Fatalf("label %q has a doubled or trailing '-'", label)
	}
}

func TestProvisionApp_EmptyOwnerOrg_Rejected(t *testing.T) {
	h := newOrchHarness(oneLiveHost())

	_, _, err := h.orch.ProvisionApp(context.Background(), FleetProvisionInput{
		ComponentID: "comp-1", Env: "prod", // no OwnerOrg
		Registry: "reg", Repository: "org/app", Digest: "sha256:abc",
		VCPU: 2, MemoryMB: 1024, Port: 8080,
	})
	if !errors.Is(err, domain.ErrFleetAppOwnerOrgRequired) {
		t.Fatalf("err = %v, want ErrFleetAppOwnerOrgRequired", err)
	}
	// Refused BEFORE anything was written: an unscoped row is exactly what the
	// guard exists to prevent, so a rejected provision must leave none behind.
	if apps, lerr := h.apps.List(context.Background()); lerr != nil || len(apps) != 0 {
		t.Fatalf("apps after rejected provision = %v (err=%v), want none", apps, lerr)
	}
	if got := h.replicas.count(); got != 0 {
		t.Fatalf("replicas after rejected provision = %d, want 0", got)
	}
}
