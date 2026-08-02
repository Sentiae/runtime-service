package usecase

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes. All of them are mutex-guarded: the restore runs in a detached
// goroutine, so the test and the use case touch them concurrently.
// ─────────────────────────────────────────────────────────────────────

type restoreVolumeRepo struct {
	mu    sync.Mutex
	byApp map[uuid.UUID][]domain.Volume
	err   error
}

func newRestoreVolumeRepo() *restoreVolumeRepo {
	return &restoreVolumeRepo{byApp: map[uuid.UUID][]domain.Volume{}}
}

func (f *restoreVolumeRepo) Create(context.Context, *domain.Volume) error { return nil }
func (f *restoreVolumeRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Volume(nil), f.byApp[appID]...), nil
}
func (f *restoreVolumeRepo) FindByID(context.Context, uuid.UUID) (*domain.Volume, error) {
	return nil, domain.ErrVolumeNotFound
}
func (f *restoreVolumeRepo) Update(_ context.Context, v *domain.Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if v.AppID == nil {
		return nil
	}
	for i := range f.byApp[*v.AppID] {
		if f.byApp[*v.AppID][i].ID == v.ID {
			f.byApp[*v.AppID][i] = *v
		}
	}
	return nil
}
func (f *restoreVolumeRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *restoreVolumeRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Volume, error) {
	return nil, nil
}

// The restorer never binds claims; the stubs exist only to satisfy the port.
func (f *restoreVolumeRepo) BindVolumesToResource(context.Context, uuid.UUID, uuid.UUID) (repository.VolumeBindResult, error) {
	return repository.VolumeBindResult{}, errors.New("not implemented")
}
func (f *restoreVolumeRepo) HasUnstampedVolumes(context.Context, uuid.UUID) (bool, error) {
	return false, errors.New("not implemented")
}

func (f *restoreVolumeRepo) statusOf(appID uuid.UUID) domain.VolumeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byApp[appID][0].Status
}

type restoreReplicaRepo struct {
	mu   sync.Mutex
	live map[uuid.UUID][]domain.Replica
	// addressless makes every replica come back resident but WITHOUT a guest
	// address — the state in which the engine cannot be probed at all.
	addressless bool
}

func newRestoreReplicaRepo() *restoreReplicaRepo {
	return &restoreReplicaRepo{live: map[uuid.UUID][]domain.Replica{}}
}

func (f *restoreReplicaRepo) Create(context.Context, *domain.Replica) error { return nil }
func (f *restoreReplicaRepo) Update(context.Context, *domain.Replica) error { return nil }
func (f *restoreReplicaRepo) FindByID(context.Context, uuid.UUID) (*domain.Replica, error) {
	return nil, domain.ErrReplicaNotFound
}
func (f *restoreReplicaRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Replica(nil), f.live[appID]...), nil
}
func (f *restoreReplicaRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Replica, error) {
	return nil, nil
}
func (f *restoreReplicaRepo) ListByState(context.Context, domain.ReplicaState) ([]domain.Replica, error) {
	return nil, nil
}
func (f *restoreReplicaRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *restoreReplicaRepo) set(appID uuid.UUID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reps := make([]domain.Replica, 0, n)
	for i := 0; i < n; i++ {
		// Resident WITH a guest address: the restore's engine-admits gate probes
		// exactly these, so a replica set without one would be untestable here.
		r := domain.Replica{
			ID: uuid.New(), AppID: appID,
			State: domain.ReplicaStateResident, GuestIP: "10.0.0.2", Port: 5432,
		}
		if f.addressless {
			r.GuestIP, r.Port = "", 0
		}
		reps = append(reps, r)
	}
	f.live[appID] = reps
}

// restoreScaler emulates the orchestrator: a scale reconciles the replica set
// synchronously, exactly like ScaleApp → ReconcileApp does.
type restoreScaler struct {
	mu       sync.Mutex
	replicas *restoreReplicaRepo
	calls    []int
	err      error
	unknown  bool
}

func (f *restoreScaler) ScaleApp(_ context.Context, appID uuid.UUID, n int) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, n)
	err, unknown := f.err, f.unknown
	f.mu.Unlock()
	if err != nil {
		return true, err
	}
	if unknown {
		return false, nil
	}
	f.replicas.set(appID, n)
	return true, nil
}

func (f *restoreScaler) scaleCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

type restoreHealth struct {
	mu      sync.Mutex
	healthy bool
	// probe, when set, decides health dynamically (e.g. "the engine only comes up
	// on the ORIGINAL bytes"), which is what exercises the rollback path.
	probe func() bool
	err   error
}

func (f *restoreHealth) Health(context.Context, string) (FleetHealthOutput, error) {
	f.mu.Lock()
	probe, healthy, err := f.probe, f.healthy, f.err
	f.mu.Unlock()
	if err != nil {
		return FleetHealthOutput{}, err
	}
	if probe != nil {
		healthy = probe()
	}
	return FleetHealthOutput{Healthy: healthy, State: "resident"}, nil
}

// restorePG stands in for the credential-free Postgres readiness probe: it
// answers whether the engine ADMITS clients, which is the claim a TCP dial
// cannot make (#p19-restore-false-green-health). Default: admits.
type restorePG struct {
	mu     sync.Mutex
	reject func() error
	calls  int
}

func (f *restorePG) probe(context.Context, string, int) error {
	f.mu.Lock()
	reject := f.reject
	f.calls++
	f.mu.Unlock()
	if reject == nil {
		return nil
	}
	return reject()
}

func (f *restorePG) probeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type restoreStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	getErr  error
}

func (f *restoreStore) Put(string, io.Reader) error { return nil }
func (f *restoreStore) Get(key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, ErrArtifactNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (f *restoreStore) Exists(string) (bool, error) { return true, nil }
func (f *restoreStore) VerifyHash(string) error     { return nil }

// ─────────────────────────────────────────────────────────────────────
// Harness
// ─────────────────────────────────────────────────────────────────────

type restoreHarness struct {
	uc       *FleetVolumeRestorer
	res      *domain.FleetResource
	rp       *domain.FleetResourceRecoveryPoint
	repo     *fakeResourceRepo
	volumes  *restoreVolumeRepo
	replicas *restoreReplicaRepo
	scaler   *restoreScaler
	health   *restoreHealth
	pg       *restorePG
	store    *restoreStore
	live     string
	dir      string
	selfHost uuid.UUID
}

const (
	liveBytes    = "ORIGINAL-VOLUME-BYTES"
	restoreBytes = "RECOVERY-POINT-BYTES"
)

func newRestoreHarness(t *testing.T) *restoreHarness {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "vol.ext4")
	if err := os.WriteFile(live, []byte(liveBytes), 0o600); err != nil {
		t.Fatal(err)
	}

	appID := uuid.New()
	resID := uuid.New()
	volID := uuid.New()
	rpID := uuid.New()
	objectKey := "volumes/" + volID.String() + "/" + rpID.String() + ".ext4"

	repo := newFakeResourceRepo()
	res := &domain.FleetResource{
		ID: resID, OwnerOrg: uuid.New(), ClaimKey: "orders-db", Env: "prod",
		Class: "postgres", Tier: resourceTierDedicated,
		Phase: domain.FleetResourcePhaseReady, AppID: &appID,
	}
	repo.seed(res)
	sum := sha256.Sum256([]byte(restoreBytes))
	rp := &domain.FleetResourceRecoveryPoint{
		ID: rpID, ResourceID: resID, VolumeID: &volID, ObjectKey: objectKey,
		Kind: "snapshot", SizeBytes: int64(len(restoreBytes)),
		Checksum: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(),
	}
	_ = repo.SaveRecoveryPoint(context.Background(), rp)

	selfHost := uuid.New()
	vols := newRestoreVolumeRepo()
	vols.byApp[appID] = []domain.Volume{{
		ID: volID, AppID: &appID, BackingPath: live, MountPath: "/data",
		Status: domain.VolumeStatusAttached, SizeMB: 1024, HostAffinity: &selfHost,
	}}
	replicas := newRestoreReplicaRepo()
	replicas.set(appID, 1)
	scaler := &restoreScaler{replicas: replicas}
	health := &restoreHealth{healthy: true}
	pg := &restorePG{}
	store := &restoreStore{objects: map[string][]byte{objectKey: []byte(restoreBytes)}}

	uc := NewFleetVolumeRestorer(context.Background(), repo, vols, replicas, scaler, health, store)
	uc.pgReady = pg.probe
	uc.drainTimeout = 2 * time.Second
	uc.drainPoll = time.Millisecond
	uc.healthTimeout = 100 * time.Millisecond
	uc.healthPoll = time.Millisecond
	uc.budget = 30 * time.Second
	// Host scope for the boot sweep, through the REAL affinity seam the reconciler
	// uses (FleetVolumeManager over the same volume rows).
	uc.SetHostScope(selfHost, NewFleetVolumeManager(vols, &recordingBackend{}, "/vol", nil))

	return &restoreHarness{
		uc: uc, res: res, rp: rp, repo: repo, volumes: vols, replicas: replicas,
		scaler: scaler, health: health, pg: pg, store: store, live: live, dir: dir,
		selfHost: selfHost,
	}
}

func (h *restoreHarness) run(t *testing.T) (RestoreResourceOutput, error) {
	t.Helper()
	out, err := h.uc.Restore(context.Background(), RestoreResourceInput{Resource: h.res, RecoveryPoint: h.rp})
	h.uc.Wait()
	return out, err
}

func (h *restoreHarness) resource(t *testing.T) *domain.FleetResource {
	t.Helper()
	r, err := h.repo.GetResourceByHandle(context.Background(), h.res.ID)
	if err != nil {
		t.Fatalf("reload resource: %v", err)
	}
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────
// Admission (the CAS phase transition + preconditions)
// ─────────────────────────────────────────────────────────────────────

func TestRestore_Admission(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(h *restoreHarness)
		wantErr    error
		wantPhase  domain.FleetResourcePhase
		wantScaled bool
	}{
		{
			name:       "from ready",
			wantPhase:  domain.FleetResourcePhaseReady, // terminal after a healthy restore
			wantScaled: true,
		},
		{
			name: "from failed",
			mutate: func(h *restoreHarness) {
				_ = h.repo.UpdateResourcePhase(context.Background(), h.res.ID, domain.FleetResourcePhaseFailed)
				h.res.Phase = domain.FleetResourcePhaseFailed
			},
			wantPhase:  domain.FleetResourcePhaseReady,
			wantScaled: true,
		},
		{
			// The durable CAS is the CROSS-PROCESS admission gate: a resource another
			// instance is restoring is never entered twice. The boot sweep is what
			// releases a restore its owner died mid-way (restoring → failed).
			name: "from restoring is refused",
			mutate: func(h *restoreHarness) {
				_ = h.repo.UpdateResourcePhase(context.Background(), h.res.ID, domain.FleetResourcePhaseRestoring)
				h.res.Phase = domain.FleetResourcePhaseRestoring
			},
			wantErr:   domain.ErrRestoreInProgress,
			wantPhase: domain.FleetResourcePhaseRestoring,
		},
		{
			// The CAS reads the DURABLE phase, not the caller's copy: a resource that
			// was torn down while this call was in flight is refused even though the
			// input still says `ready`. (This case used to use the `pending` phase,
			// which no code ever wrote — it is retired.)
			name: "a phase outside the admit set is refused by the durable CAS",
			mutate: func(h *restoreHarness) {
				_ = h.repo.UpdateResourcePhase(context.Background(), h.res.ID, domain.FleetResourcePhaseDecommissioned)
			},
			wantErr:   domain.ErrRestoreInProgress,
			wantPhase: domain.FleetResourcePhaseDecommissioned,
		},
		{
			name: "tombstoned resource is refused (fork territory)",
			mutate: func(h *restoreHarness) {
				h.res.Phase = domain.FleetResourcePhaseDecommissioned
			},
			wantErr:   domain.ErrRestoreNoBackingApp,
			wantPhase: domain.FleetResourcePhaseReady,
		},
		{
			name:      "shared tier is refused",
			mutate:    func(h *restoreHarness) { h.res.Tier = "shared" },
			wantErr:   domain.ErrResourceTierUnsupported,
			wantPhase: domain.FleetResourcePhaseReady,
		},
		{
			name:      "resource with no app is refused",
			mutate:    func(h *restoreHarness) { h.res.AppID = nil },
			wantErr:   domain.ErrRestoreNoBackingApp,
			wantPhase: domain.FleetResourcePhaseReady,
		},
		{
			name: "two volumes is refused",
			mutate: func(h *restoreHarness) {
				appID := *h.res.AppID
				h.volumes.byApp[appID] = append(h.volumes.byApp[appID], domain.Volume{
					ID: uuid.New(), AppID: &appID, BackingPath: filepath.Join(h.dir, "vol2.ext4"),
				})
			},
			wantErr:   domain.ErrRestoreVolumeAmbiguous,
			wantPhase: domain.FleetResourcePhaseReady,
		},
		{
			name: "recovery point of another resource is refused",
			mutate: func(h *restoreHarness) {
				h.rp.ResourceID = uuid.New()
			},
			wantErr:   domain.ErrRecoveryPointNotFound,
			wantPhase: domain.FleetResourcePhaseReady,
		},
		{
			name:      "no artifact store is refused",
			mutate:    func(h *restoreHarness) { h.uc.store = nil },
			wantErr:   domain.ErrRestoreStoreUnavailable,
			wantPhase: domain.FleetResourcePhaseReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			if tt.mutate != nil {
				tt.mutate(h)
			}
			out, err := h.run(t)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && out.Phase != string(domain.FleetResourcePhaseRestoring) {
				t.Fatalf("admitted restore must return phase restoring, got %q", out.Phase)
			}
			if got := h.resource(t).Phase; got != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", got, tt.wantPhase)
			}
			if scaled := len(h.scaler.scaleCalls()) > 0; scaled != tt.wantScaled {
				t.Fatalf("app scaled = %v, want %v (a refused restore must never touch the live engine)", scaled, tt.wantScaled)
			}
		})
	}
}

// A second Restore while one is live is refused by the in-process claim, even
// though the durable CAS admits `restoring` (that is for post-crash re-issue).
func TestRestore_DoubleEntryRefused(t *testing.T) {
	h := newRestoreHarness(t)
	release := make(chan struct{})
	h.store.mu.Lock() // block the background goroutine inside store.Get
	go func() {
		<-release
		h.store.mu.Unlock()
	}()

	if _, err := h.uc.Restore(context.Background(), RestoreResourceInput{Resource: h.res, RecoveryPoint: h.rp}); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	_, err := h.uc.Restore(context.Background(), RestoreResourceInput{Resource: h.res, RecoveryPoint: h.rp})
	if !errors.Is(err, domain.ErrRestoreInProgress) {
		t.Fatalf("second restore: err = %v, want ErrRestoreInProgress", err)
	}
	close(release)
	h.uc.Wait()
}

// ─────────────────────────────────────────────────────────────────────
// The download verifier
// ─────────────────────────────────────────────────────────────────────

func TestVerifyRecoveryPoint(t *testing.T) {
	sum := sha256.Sum256([]byte(restoreBytes))
	good := hex.EncodeToString(sum[:])

	tests := []struct {
		name    string
		rp      domain.FleetResourceRecoveryPoint
		staged  int64
		sum     string
		wantErr bool
	}{
		{"size + checksum match", domain.FleetResourceRecoveryPoint{SizeBytes: 20, Checksum: good}, 20, good, false},
		{"size mismatch", domain.FleetResourceRecoveryPoint{SizeBytes: 21, Checksum: good}, 20, good, true},
		{"checksum mismatch", domain.FleetResourceRecoveryPoint{SizeBytes: 20, Checksum: good}, 20, strings.Repeat("a", 64), true},
		{"checksum case-insensitive", domain.FleetResourceRecoveryPoint{SizeBytes: 20, Checksum: strings.ToUpper(good)}, 20, good, false},
		{"legacy point: size only", domain.FleetResourceRecoveryPoint{SizeBytes: 20}, 20, good, false},
		{"legacy point: size mismatch still fails", domain.FleetResourceRecoveryPoint{SizeBytes: 20}, 19, good, true},
		{"nothing verifiable is refused", domain.FleetResourceRecoveryPoint{}, 20, good, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := tt.rp
			rp.ID = uuid.New()
			err := verifyRecoveryPoint(context.Background(), &rp, tt.staged, tt.sum)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrRestoreIntegrity) {
					t.Fatalf("err = %v, want ErrRestoreIntegrity", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// A corrupt recovery point must be caught BEFORE the live volume is touched.
func TestRestore_IntegrityFailure_LeavesLiveVolumeUntouched(t *testing.T) {
	h := newRestoreHarness(t)
	h.store.objects[h.rp.ObjectKey] = []byte("CORRUPT-BYTES-OF-THE-SAME-LENGTH!!")

	if _, err := h.run(t); err != nil {
		t.Fatalf("restore admission: %v", err)
	}
	if got := readFile(t, h.live); got != liveBytes {
		t.Fatalf("live volume = %q, want the ORIGINAL %q", got, liveBytes)
	}
	if len(h.scaler.scaleCalls()) != 0 {
		t.Fatalf("a failed verification must not stop the engine, scaled %v", h.scaler.scaleCalls())
	}
	res := h.resource(t)
	if res.Phase != domain.FleetResourcePhaseReady {
		t.Fatalf("phase = %q, want the pre-restore ready", res.Phase)
	}
	if res.LastError == "" {
		t.Fatal("last_error must record why the restore aborted")
	}
	if h.volumes.statusOf(*h.res.AppID) == domain.VolumeStatusRestoring {
		t.Fatal("volume must not be left in the restoring stand-off")
	}
	entries, _ := os.ReadDir(h.dir)
	if len(entries) != 1 {
		t.Fatalf("staging file not cleaned up: %d entries", len(entries))
	}
}

// ─────────────────────────────────────────────────────────────────────
// The full swap, and the rollback when the restored volume will not boot
// ─────────────────────────────────────────────────────────────────────

func TestRestore_HappyPath(t *testing.T) {
	h := newRestoreHarness(t)

	out, err := h.run(t)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if out.Phase != string(domain.FleetResourcePhaseRestoring) {
		t.Fatalf("admission phase = %q", out.Phase)
	}
	if got := readFile(t, h.live); got != restoreBytes {
		t.Fatalf("live volume = %q, want the RESTORED %q", got, restoreBytes)
	}
	if _, err := os.Stat(h.live + prerestoreSuffix); !os.IsNotExist(err) {
		t.Fatal("the pre-restore volume must be removed once the restore boots healthy")
	}
	if calls := h.scaler.scaleCalls(); len(calls) != 2 || calls[0] != 0 || calls[1] != 1 {
		t.Fatalf("scale calls = %v, want [0 1]", calls)
	}
	res := h.resource(t)
	if res.Phase != domain.FleetResourcePhaseReady {
		t.Fatalf("phase = %q, want ready", res.Phase)
	}
	if res.LastError != "" {
		t.Fatalf("last_error = %q, want empty on a clean restore", res.LastError)
	}
	if st := h.volumes.statusOf(*h.res.AppID); st != domain.VolumeStatusAvailable {
		t.Fatalf("volume status = %q, want available", st)
	}
	rps, _ := h.repo.ListRecoveryPoints(context.Background(), h.res.ID)
	if len(rps) != 1 || !rps[0].RestoredInPlaceOK {
		t.Fatalf("recovery point must be marked restored-in-place after a clean restore, got %+v", rps)
	}
}

// The recovery path that matters most: the backing file is GONE (dead disk,
// stray delete, half-finished teardown) and a good recovery point exists. This
// used to fail at the park-the-original rename — a resource was unrecoverable
// with its own restore sitting right there.
func TestRestore_RecoversAVolumeWhoseBackingFileIsGone(t *testing.T) {
	t.Run("restores onto the missing file", func(t *testing.T) {
		h := newRestoreHarness(t)
		if err := os.Remove(h.live); err != nil {
			t.Fatal(err)
		}

		if _, err := h.run(t); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if got := readFile(t, h.live); got != restoreBytes {
			t.Fatalf("live volume = %q, want the RESTORED %q", got, restoreBytes)
		}
		if _, err := os.Stat(h.live + prerestoreSuffix); !os.IsNotExist(err) {
			t.Fatal("no anchor may be created: there was no original to park")
		}
		res := h.resource(t)
		if res.Phase != domain.FleetResourcePhaseReady {
			t.Fatalf("phase = %q, want ready", res.Phase)
		}
		if res.LastError != "" {
			t.Fatalf("last_error = %q, want empty on a clean restore", res.LastError)
		}
	})

	// The other half of the same state: with no original there is no rollback, so
	// a restore that will not boot must END degraded and SAY why — never silently
	// ready, never pretending an original was put back.
	t.Run("cannot roll back what never existed, and says so", func(t *testing.T) {
		h := newRestoreHarness(t)
		if err := os.Remove(h.live); err != nil {
			t.Fatal(err)
		}
		h.health.healthy = false

		if _, err := h.run(t); err != nil {
			t.Fatalf("restore: %v", err)
		}
		res := h.resource(t)
		if res.Phase != domain.FleetResourcePhaseDegraded {
			t.Fatalf("phase = %q, want degraded", res.Phase)
		}
		if !strings.Contains(res.LastError, domain.ErrRestoreNoPrerestoreAnchor.Error()) {
			t.Fatalf("last_error = %q, want it to name the missing pre-restore anchor", res.LastError)
		}
		// The restored bytes stay in place: they are the only data there is.
		if got := readFile(t, h.live); got != restoreBytes {
			t.Fatalf("live volume = %q, want the restored bytes left in place", got)
		}
	})
}

func TestRestore_RollsBackWhenRestoredVolumeWillNotBoot(t *testing.T) {
	h := newRestoreHarness(t)
	// The engine only comes up on the ORIGINAL bytes: the recovery point is
	// unbootable, the pre-restore volume is fine.
	h.health.probe = func() bool {
		b, err := os.ReadFile(h.live)
		return err == nil && string(b) == liveBytes
	}

	if _, err := h.run(t); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readFile(t, h.live); got != liveBytes {
		t.Fatalf("live volume = %q, want the ORIGINAL %q back", got, liveBytes)
	}
	failed := h.live + ".failed-" + h.rp.ID.String()
	if got := readFile(t, failed); got != restoreBytes {
		t.Fatalf("the failed restore must be kept for forensics, got %q", got)
	}
	res := h.resource(t)
	// The engine is back in service on its original data → ready, but last_error
	// is what tells a poller the restore did NOT take.
	if res.Phase != domain.FleetResourcePhaseReady {
		t.Fatalf("phase = %q, want ready after a successful rollback", res.Phase)
	}
	if res.LastError == "" {
		t.Fatal("a rolled-back restore MUST leave last_error set, else it is indistinguishable from success")
	}
	if st := h.volumes.statusOf(*h.res.AppID); st != domain.VolumeStatusAvailable {
		t.Fatalf("volume status = %q, want available", st)
	}
	rps, _ := h.repo.ListRecoveryPoints(context.Background(), h.res.ID)
	if rps[0].RestoredInPlaceOK {
		t.Fatal("a recovery point that would not boot must NOT be marked restored-in-place")
	}
}

// When even the pre-restore volume will not boot, the resource is degraded —
// never silently ready.
func TestRestore_DegradedWhenRollbackAlsoFailsToBoot(t *testing.T) {
	h := newRestoreHarness(t)
	h.health.healthy = false // nothing comes up, restored or original

	if _, err := h.run(t); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readFile(t, h.live); got != liveBytes {
		t.Fatalf("live volume = %q, want the ORIGINAL %q back even when it will not boot", got, liveBytes)
	}
	res := h.resource(t)
	if res.Phase != domain.FleetResourcePhaseDegraded {
		t.Fatalf("phase = %q, want degraded", res.Phase)
	}
	if res.LastError == "" {
		t.Fatal("last_error must be set")
	}
}

// The defect this closes (#p19-restore-false-green-health): the restored engine
// is ALIVE and its port ACCEPTS connections — fleet health is green — yet it
// refuses every client because the restored pg_hba.conf came back torn. Twice
// live, that combination produced phase=ready, verified=true and an EMPTY
// last_error over a database nobody could reach.
func TestRestore_NotSuccessfulWhenTheEngineRefusesEveryClient(t *testing.T) {
	hbaErr := errors.New(`postgres at 10.0.0.2:5432 refused the connection before authentication: FATAL: SQLSTATE 28000: no pg_hba.conf entry for host "10.0.0.1"`)

	tests := []struct {
		name string
		// reject decides the probe's verdict; live is the current backing file.
		reject    func(live string) func() error
		wantPhase domain.FleetResourcePhase
		wantBytes string
	}{
		{
			// The recovery point's pg_hba is torn, the original's is fine: the
			// restore must roll back rather than report success.
			name: "torn pg_hba in the recovery point rolls the restore back",
			reject: func(live string) func() error {
				return func() error {
					b, err := os.ReadFile(live)
					if err == nil && string(b) == restoreBytes {
						return hbaErr
					}
					return nil
				}
			},
			wantPhase: domain.FleetResourcePhaseReady, // back in service on the ORIGINAL
			wantBytes: liveBytes,
		},
		{
			// Nothing admits clients, restored or original: degraded, never ready.
			name: "an engine that never admits leaves the resource degraded",
			reject: func(string) func() error {
				return func() error { return hbaErr }
			},
			wantPhase: domain.FleetResourcePhaseDegraded,
			wantBytes: liveBytes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			// Fleet health is GREEN throughout — process alive, port accepting.
			// Only the engine-admits gate can tell the difference.
			h.health.healthy = true
			h.pg.reject = tt.reject(h.live)

			if _, err := h.run(t); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if h.pg.probeCalls() == 0 {
				t.Fatal("the restore never probed the engine; a TCP dial alone cannot declare a restore successful")
			}
			if got := readFile(t, h.live); got != tt.wantBytes {
				t.Fatalf("live volume = %q, want %q", got, tt.wantBytes)
			}
			res := h.resource(t)
			if res.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", res.Phase, tt.wantPhase)
			}
			if !strings.Contains(res.LastError, "no pg_hba.conf entry") {
				t.Fatalf("last_error = %q, must surface why the engine refused clients", res.LastError)
			}
			rps, _ := h.repo.ListRecoveryPoints(context.Background(), h.res.ID)
			if rps[0].RestoredInPlaceOK {
				t.Fatal("a recovery point whose engine admits nobody must NOT be marked restored-in-place")
			}
		})
	}
}

// A resident replica with no guest address cannot be probed, so the restore is
// unprovable — and an unprovable restore is a failed one.
func TestRestore_FailsClosedWhenTheEngineCannotBeProbed(t *testing.T) {
	h := newRestoreHarness(t)
	h.health.healthy = true
	h.replicas.mu.Lock()
	h.replicas.addressless = true
	h.replicas.mu.Unlock()

	if _, err := h.run(t); err != nil {
		t.Fatalf("restore: %v", err)
	}
	res := h.resource(t)
	if res.Phase != domain.FleetResourcePhaseDegraded {
		t.Fatalf("phase = %q, want degraded when the engine could not be probed at all", res.Phase)
	}
	if !strings.Contains(res.LastError, "no guest address") {
		t.Fatalf("last_error = %q, must say the engine could not be probed", res.LastError)
	}
	rps, _ := h.repo.ListRecoveryPoints(context.Background(), h.res.ID)
	if rps[0].RestoredInPlaceOK {
		t.Fatal("an unprobeable restore must NOT mark its recovery point restored-in-place")
	}
}

// ─────────────────────────────────────────────────────────────────────
// The swap file-state machine, driven directly against plain files.
// ─────────────────────────────────────────────────────────────────────

func TestSwapIn_FileStates(t *testing.T) {
	tests := []struct {
		name string
		// setup writes the starting file states; "" means absent.
		live, pre string
		wantLive  string
		wantPre   string
	}{
		{
			name:     "first restore parks the original",
			live:     "ORIGINAL",
			pre:      "",
			wantLive: "STAGED",
			wantPre:  "ORIGINAL", // parked, removed only after a healthy boot
		},
		{
			name:     "interrupted restore never clobbers the anchor",
			live:     "HALF-RESTORED",
			pre:      "ORIGINAL",
			wantLive: "STAGED",
			wantPre:  "ORIGINAL",
		},
		{
			name:     "interrupted after park: live missing, anchor kept",
			live:     "",
			pre:      "ORIGINAL",
			wantLive: "STAGED",
			wantPre:  "ORIGINAL",
		},
		{
			// The lost-backing-file case, and the reason Restore is worth having: a
			// volume whose file is gone, a good recovery point in hand. Nothing to
			// park means nothing to lose — both paths are established absent, so the
			// install cannot overwrite any surviving copy of the data.
			name:     "no live and no anchor installs the recovery point",
			live:     "",
			pre:      "",
			wantLive: "STAGED",
			wantPre:  "", // no anchor was created: there was no original to park
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			live := filepath.Join(dir, "vol.ext4")
			pre := live + prerestoreSuffix
			staged := filepath.Join(dir, ".restore.tmp")
			if err := os.WriteFile(staged, []byte("STAGED"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.live != "" {
				if err := os.WriteFile(live, []byte(tt.live), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.pre != "" {
				if err := os.WriteFile(pre, []byte(tt.pre), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := swapIn(staged, live, pre)
			if err != nil {
				t.Fatalf("swapIn: %v", err)
			}
			if got := readFile(t, live); got != tt.wantLive {
				t.Fatalf("live = %q, want %q", got, tt.wantLive)
			}
			if tt.wantPre == "" {
				if _, serr := os.Stat(pre); !os.IsNotExist(serr) {
					t.Fatal("pre-restore file must be absent")
				}
				return
			}
			if got := readFile(t, pre); got != tt.wantPre {
				t.Fatalf("pre-restore = %q, want %q", got, tt.wantPre)
			}
			if _, serr := os.Stat(staged); !os.IsNotExist(serr) {
				t.Fatal("staged file must be consumed by the rename")
			}
		})
	}
}

func TestSwapBack_FileStates(t *testing.T) {
	t.Run("reinstates the original and keeps the failed restore", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		pre := live + prerestoreSuffix
		failed := live + ".failed-x"
		if err := os.WriteFile(live, []byte("RESTORED"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pre, []byte("ORIGINAL"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := swapBack(live, pre, failed); err != nil {
			t.Fatalf("swapBack: %v", err)
		}
		if got := readFile(t, live); got != "ORIGINAL" {
			t.Fatalf("live = %q, want ORIGINAL", got)
		}
		if got := readFile(t, failed); got != "RESTORED" {
			t.Fatalf("failed = %q, want RESTORED", got)
		}
		if _, err := os.Stat(pre); !os.IsNotExist(err) {
			t.Fatal("the anchor is consumed by the rollback")
		}
	})

	// A ROLLBACK with no anchor is terminal, unlike the forward swap: the anchor
	// holds the only copy of the pre-restore data, so there is nothing to
	// reinstate and no retry can produce one. It must say so unambiguously.
	t.Run("refuses without an anchor", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		if err := os.WriteFile(live, []byte("RESTORED"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := swapBack(live, live+prerestoreSuffix, live+".failed-x")
		if !errors.Is(err, domain.ErrRestoreNoPrerestoreAnchor) {
			t.Fatalf("err = %v, want ErrRestoreNoPrerestoreAnchor", err)
		}
		if got := readFile(t, live); got != "RESTORED" {
			t.Fatalf("live = %q, want it untouched when the rollback cannot proceed", got)
		}
	})
}

func TestRestorePrerestore(t *testing.T) {
	t.Run("puts the parked original back when live is missing", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		pre := live + prerestoreSuffix
		if err := os.WriteFile(pre, []byte("ORIGINAL"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := restorePrerestore(live, pre); err != nil {
			t.Fatalf("restorePrerestore: %v", err)
		}
		if got := readFile(t, live); got != "ORIGINAL" {
			t.Fatalf("live = %q, want ORIGINAL", got)
		}
	})

	// Reaching here with neither file means the swap failed on a volume whose
	// backing file was already lost: there is nothing to put back, and the caller
	// must degrade rather than report a recovery that did not happen.
	t.Run("reports the terminal anchor error when there is nothing to put back", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		err := restorePrerestore(live, live+prerestoreSuffix)
		if !errors.Is(err, domain.ErrRestoreNoPrerestoreAnchor) {
			t.Fatalf("err = %v, want ErrRestoreNoPrerestoreAnchor", err)
		}
	})

	t.Run("leaves an in-place live file alone", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		pre := live + prerestoreSuffix
		if err := os.WriteFile(live, []byte("LIVE"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pre, []byte("ORIGINAL"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := restorePrerestore(live, pre); err != nil {
			t.Fatalf("restorePrerestore: %v", err)
		}
		if got := readFile(t, live); got != "LIVE" {
			t.Fatalf("live = %q, want LIVE untouched", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────
// Boot-time sweep
// ─────────────────────────────────────────────────────────────────────

// seedStuckRestore adds a resource in phase restoring whose app has one volume
// pinned to `host` (nil host = unpinned). Returns the resource id.
func (h *restoreHarness) seedStuckRestore(t *testing.T, host *uuid.UUID, withApp bool) uuid.UUID {
	t.Helper()
	res := &domain.FleetResource{
		ID: uuid.New(), OwnerOrg: uuid.New(), ClaimKey: uuid.NewString(), Env: "prod",
		Tier: resourceTierDedicated, Phase: domain.FleetResourcePhaseRestoring,
	}
	if withApp {
		appID := uuid.New()
		res.AppID = &appID
		h.volumes.mu.Lock()
		h.volumes.byApp[appID] = []domain.Volume{{
			ID: uuid.New(), AppID: &appID, BackingPath: "/vol/other.ext4",
			Status: domain.VolumeStatusRestoring, HostAffinity: host,
		}}
		h.volumes.mu.Unlock()
	}
	h.repo.seed(res)
	return res.ID
}

func TestSweepInterruptedRestores(t *testing.T) {
	otherHost := uuid.New()

	tests := []struct {
		name string
		// seed returns the resource id under test.
		seed        func(t *testing.T, h *restoreHarness) uuid.UUID
		unscoped    bool
		wantSwept   bool
		wantPhase   domain.FleetResourcePhase
		wantLastErr string
	}{
		{
			name: "this host's stuck restore is released to failed",
			seed: func(t *testing.T, h *restoreHarness) uuid.UUID {
				return h.seedStuckRestore(t, &h.selfHost, true)
			},
			wantSwept:   true,
			wantPhase:   domain.FleetResourcePhaseFailed,
			wantLastErr: restoreInterruptedMsg,
		},
		{
			// The hazard this scoping exists for: an instance booting on host X must
			// never touch a restore that is LIVE on host Y.
			name: "another host's live restore is left alone",
			seed: func(t *testing.T, h *restoreHarness) uuid.UUID {
				return h.seedStuckRestore(t, &otherHost, true)
			},
			wantPhase: domain.FleetResourcePhaseRestoring,
		},
		{
			name: "unpinned volume: ownership undetermined, left alone",
			seed: func(t *testing.T, h *restoreHarness) uuid.UUID {
				return h.seedStuckRestore(t, nil, true)
			},
			wantPhase: domain.FleetResourcePhaseRestoring,
		},
		{
			name: "no backing app: ownership undetermined, left alone",
			seed: func(t *testing.T, h *restoreHarness) uuid.UUID {
				return h.seedStuckRestore(t, nil, false)
			},
			wantPhase: domain.FleetResourcePhaseRestoring,
		},
		{
			name: "no host scope wired: sweep touches nothing",
			seed: func(t *testing.T, h *restoreHarness) uuid.UUID {
				return h.seedStuckRestore(t, &h.selfHost, true)
			},
			unscoped:  true,
			wantPhase: domain.FleetResourcePhaseRestoring,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRestoreHarness(t)
			if tt.unscoped {
				h.uc.SetHostScope(uuid.Nil, nil)
			}
			id := tt.seed(t, h)

			n, err := h.uc.SweepInterruptedRestores(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			want := 0
			if tt.wantSwept {
				want = 1
			}
			if n != want {
				t.Fatalf("released = %d, want %d", n, want)
			}
			got, _ := h.repo.GetResourceByHandle(context.Background(), id)
			if got.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tt.wantPhase)
			}
			if got.LastError != tt.wantLastErr {
				t.Fatalf("last_error = %q, want %q", got.LastError, tt.wantLastErr)
			}
		})
	}
}

// A released (swept) resource is restorable again — that is the whole point of
// moving it out of `restoring`.
func TestSweepInterruptedRestores_ReleasedResourceIsRestorableAgain(t *testing.T) {
	h := newRestoreHarness(t)
	if _, err := h.repo.CompareAndSwapPhase(context.Background(), h.res.ID,
		[]domain.FleetResourcePhase{domain.FleetResourcePhaseReady}, domain.FleetResourcePhaseRestoring); err != nil {
		t.Fatal(err)
	}

	if _, err := h.uc.SweepInterruptedRestores(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	released := h.resource(t)
	if released.Phase != domain.FleetResourcePhaseFailed {
		t.Fatalf("phase = %q, want failed", released.Phase)
	}

	// Re-issue with the freshly-read row, exactly as the handler would.
	h.res = released
	if _, err := h.run(t); err != nil {
		t.Fatalf("re-issued restore was refused: %v", err)
	}
	if got := readFile(t, h.live); got != restoreBytes {
		t.Fatalf("live volume = %q, want the RESTORED %q", got, restoreBytes)
	}
	if h.resource(t).Phase != domain.FleetResourcePhaseReady {
		t.Fatalf("phase = %q, want ready", h.resource(t).Phase)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Boot-time sweep: reclaiming abandoned staging files
// ─────────────────────────────────────────────────────────────────────

// stuckRestoring leaves the harness resource in phase restoring, exactly as a
// restore killed mid-flight would have.
func (h *restoreHarness) stuckRestoring(t *testing.T) {
	t.Helper()
	if _, err := h.repo.CompareAndSwapPhase(context.Background(), h.res.ID,
		[]domain.FleetResourcePhase{domain.FleetResourcePhaseReady}, domain.FleetResourcePhaseRestoring); err != nil {
		t.Fatal(err)
	}
}

// seedStagedStuckRestore adds a SECOND stuck resource of this host, on its own
// volume directory, whose one recovery point has a staging file on disk.
func (h *restoreHarness) seedStagedStuckRestore(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	dir := t.TempDir()
	appID, resID, rpID := uuid.New(), uuid.New(), uuid.New()
	h.repo.seed(&domain.FleetResource{
		ID: resID, OwnerOrg: uuid.New(), ClaimKey: uuid.NewString(), Env: "prod",
		Tier: resourceTierDedicated, Phase: domain.FleetResourcePhaseRestoring, AppID: &appID,
	})
	if err := h.repo.SaveRecoveryPoint(context.Background(), &domain.FleetResourceRecoveryPoint{
		ID: rpID, ResourceID: resID, ObjectKey: "volumes/" + rpID.String() + ".ext4", Kind: "snapshot",
	}); err != nil {
		t.Fatal(err)
	}
	h.volumes.mu.Lock()
	h.volumes.byApp[appID] = []domain.Volume{{
		ID: uuid.New(), AppID: &appID, BackingPath: filepath.Join(dir, "vol.ext4"),
		Status: domain.VolumeStatusRestoring, HostAffinity: &h.selfHost,
	}}
	h.volumes.mu.Unlock()
	staged := restoreStagingPath(dir, rpID)
	if err := os.WriteFile(staged, []byte("STAGED-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	return resID, staged
}

// A restore killed mid-copy (panic, process kill) never reaches the branches
// that remove `staged`, and what it leaves is the size of the WHOLE volume. The
// boot sweep is the only thing that can reclaim it.
func TestSweepInterruptedRestores_ReclaimsAbandonedStagingFile(t *testing.T) {
	h := newRestoreHarness(t)
	h.stuckRestoring(t)
	staged := restoreStagingPath(h.dir, h.rp.ID)
	if err := os.WriteFile(staged, []byte("HALF-COPIED-VOLUME"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A staging file of ANOTHER volume's restore, in the SAME shared directory —
	// it may be live, and reclaiming by recovery-point id must never reach it.
	// This is what a `.restore-*.tmp` glob would have destroyed.
	foreign := restoreStagingPath(h.dir, uuid.New())
	if err := os.WriteFile(foreign, []byte("ANOTHER-VOLUMES-IN-FLIGHT-RESTORE"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := h.uc.SweepInterruptedRestores(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The int is restores RELEASED, not files removed.
	if n != 1 {
		t.Fatalf("released = %d, want 1", n)
	}
	if _, serr := os.Stat(staged); !os.IsNotExist(serr) {
		t.Fatalf("abandoned staging file not reclaimed: %v", serr)
	}
	if got := readFile(t, foreign); got != "ANOTHER-VOLUMES-IN-FLIGHT-RESTORE" {
		t.Fatalf("foreign staging file = %q, want it untouched", got)
	}
	if got := readFile(t, h.live); got != liveBytes {
		t.Fatalf("live volume = %q, want it untouched", got)
	}
}

// The normal case by far: the restore exited through one of the branches that
// already removes its staging file. A missing file is not an error.
func TestSweepInterruptedRestores_NoStagingFileIsNotAnError(t *testing.T) {
	h := newRestoreHarness(t)
	h.stuckRestoring(t)

	n, err := h.uc.SweepInterruptedRestores(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("released = %d, want 1", n)
	}
	if h.resource(t).Phase != domain.FleetResourcePhaseFailed {
		t.Fatalf("phase = %q, want failed", h.resource(t).Phase)
	}
}

// Reclaiming is best-effort: it must never cost the sweep its actual job
// (releasing stuck restores) nor the reclaim of any OTHER resource's file. A
// non-empty directory at the staging path fails os.Remove with something that is
// not IsNotExist, which is the failure mode to prove.
func TestSweepInterruptedRestores_ReclaimFailureDoesNotAbortTheSweep(t *testing.T) {
	h := newRestoreHarness(t)
	h.stuckRestoring(t)
	unremovable := restoreStagingPath(h.dir, h.rp.ID)
	if err := os.MkdirAll(filepath.Join(unremovable, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	otherID, otherStaged := h.seedStagedStuckRestore(t)

	n, err := h.uc.SweepInterruptedRestores(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("released = %d, want 2", n)
	}
	if h.resource(t).Phase != domain.FleetResourcePhaseFailed {
		t.Fatalf("phase = %q, want failed", h.resource(t).Phase)
	}
	other, err := h.repo.GetResourceByHandle(context.Background(), otherID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Phase != domain.FleetResourcePhaseFailed {
		t.Fatalf("second resource phase = %q, want failed", other.Phase)
	}
	if _, serr := os.Stat(otherStaged); !os.IsNotExist(serr) {
		t.Fatalf("a failed reclaim stopped the next one: %v", serr)
	}
	if _, serr := os.Stat(unremovable); serr != nil {
		t.Fatalf("the unremovable path should be left as it was: %v", serr)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Staging: compressed transfer, sparse materialization
// ─────────────────────────────────────────────────────────────────────

// Snapshots are stored gzipped (holes transfer for nothing), but objects
// written before that landed are RAW images and are still a customer's only
// recovery points — so the format is sniffed from the gzip magic and both
// restore identically. The checksum is verified over the bytes AS DOWNLOADED
// and the size over the DECOMPRESSED image; either failing refuses the object.
func TestRestore_StageAcceptsRawAndGzippedObjects(t *testing.T) {
	image := []byte(restoreBytes)
	gz := gzipBytes(t, image)

	tests := []struct {
		name string
		// object is what the store holds; summed is what the recovery point's
		// checksum is computed over (a mismatch models a corrupt transfer).
		object   []byte
		summed   []byte
		size     int64
		wantErr  bool
		wantFile string
	}{
		{"gzipped object", gz, gz, int64(len(image)), false, string(image)},
		{"raw pre-compression object still restores", image, image, int64(len(image)), false, string(image)},
		{"corrupt gzipped object refused", gz, image, int64(len(image)), true, ""},
		{"size mismatch refused", gz, gz, int64(len(image)) + 1, true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "staged.tmp")
			sum := sha256.Sum256(tt.summed)
			rp := &domain.FleetResourceRecoveryPoint{
				ID: uuid.New(), ObjectKey: "k", SizeBytes: tt.size,
				Checksum: hex.EncodeToString(sum[:]),
			}
			uc := &FleetVolumeRestorer{store: &restoreStore{objects: map[string][]byte{"k": tt.object}}}

			err := uc.stage(context.Background(), rp, dst)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrRestoreIntegrity) {
					t.Fatalf("err = %v, want ErrRestoreIntegrity", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			if got := readFile(t, dst); got != tt.wantFile {
				t.Fatalf("staged = %q, want %q", got, tt.wantFile)
			}
		})
	}
}

// The full loop the product depends on: a SPARSE volume is snapshotted, stored,
// and staged back. The bytes must come back exactly — including the holes — and
// neither the transfer nor the staged file may cost the volume's nominal size.
// Before compression a 20GB-nominal volume transferred all 20GB and could not be
// snapshotted (nor, therefore, deleted) at all.
func TestRestore_RoundTripFromSnapshotPreservesHolesAndBytes(t *testing.T) {
	const (
		nominal = 8 << 20
		head    = 1 << 20
		tail    = 1 << 10
	)
	// The volume: data, a big hole, then data again at the very end.
	image := make([]byte, nominal)
	copy(image, bytes.Repeat([]byte{0x5A}, head))
	copy(image[nominal-tail:], bytes.Repeat([]byte{0x7E}, tail))

	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	// The volume itself is sparse: the snapshot streams THIS file (there is no
	// staging copy any more), so the holes are read straight off it.
	writeSparseImage(t, h.backing, image, head, tail)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rp := points[0]
	body := h.store.stored(rp.ObjectKey)
	if int64(len(body)) >= nominal/10 {
		t.Fatalf("stored %d bytes for a %d-byte volume: the holes are still being transferred", len(body), nominal)
	}

	dst := filepath.Join(t.TempDir(), "staged.tmp")
	uc := &FleetVolumeRestorer{store: &restoreStore{objects: map[string][]byte{rp.ObjectKey: body}}}
	if err := uc.stage(context.Background(), &rp, dst); err != nil {
		t.Fatalf("stage: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !bytes.Equal(got, image) {
		t.Fatalf("staged image differs from the volume (len %d, want %d)", len(got), len(image))
	}
	// Sparseness is the other half: materializing the holes would reproduce on
	// restore exactly the disk cost the snapshot side eliminates. Self-calibrating
	// — a filesystem that does not do sparse files cannot be asked to prove it.
	if !sparseSupported(t) {
		t.Log("filesystem does not keep files sparse; skipping the allocation check")
		return
	}
	if alloc := allocatedBytes(t, dst); alloc >= nominal/2 {
		t.Fatalf("staged file allocates %d bytes of a %d-byte image — the holes were materialized", alloc, int64(nominal))
	}
}

// writeSparseImage materializes image at path as a genuinely sparse file: head
// bytes written at the front, tail bytes written at the very end, and a hole in
// between (Truncate out to the full length).
func writeSparseImage(t *testing.T, path string, image []byte, head, tail int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	nominal := int64(len(image))
	if _, err := f.WriteAt(image[:head], 0); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := f.WriteAt(image[len(image)-tail:], nominal-int64(tail)); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	if err := f.Truncate(nominal); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// gzipBytes compresses b the way the snapshot uploader does.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// allocatedBytes reports the disk actually charged to a file (st_blocks × 512),
// which is what distinguishes a hole from a run of written zeros.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t", path)
	}
	return sys.Blocks * 512
}

// sparseSupported reports whether the test filesystem leaves a hole unallocated.
func sparseSupported(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "sparse.probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create probe: %v", err)
	}
	if _, err := f.WriteAt([]byte{1}, 8<<20); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
	return allocatedBytes(t, probe) < 4<<20
}
