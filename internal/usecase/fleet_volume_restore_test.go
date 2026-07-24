package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
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
	for i := range f.byApp[v.AppID] {
		if f.byApp[v.AppID][i].ID == v.ID {
			f.byApp[v.AppID][i] = *v
		}
	}
	return nil
}
func (f *restoreVolumeRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *restoreVolumeRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Volume, error) {
	return nil, nil
}
func (f *restoreVolumeRepo) statusOf(appID uuid.UUID) domain.VolumeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byApp[appID][0].Status
}

type restoreReplicaRepo struct {
	mu   sync.Mutex
	live map[uuid.UUID][]domain.Replica
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
		reps = append(reps, domain.Replica{ID: uuid.New(), AppID: appID})
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
		ID: volID, AppID: appID, BackingPath: live, MountPath: "/data",
		Status: domain.VolumeStatusAttached, SizeMB: 1024, HostAffinity: &selfHost,
	}}
	replicas := newRestoreReplicaRepo()
	replicas.set(appID, 1)
	scaler := &restoreScaler{replicas: replicas}
	health := &restoreHealth{healthy: true}
	store := &restoreStore{objects: map[string][]byte{objectKey: []byte(restoreBytes)}}

	uc := NewFleetVolumeRestorer(context.Background(), repo, vols, replicas, scaler, health, store)
	uc.drainTimeout = 2 * time.Second
	uc.drainPoll = time.Millisecond
	uc.healthTimeout = 100 * time.Millisecond
	uc.healthPoll = time.Millisecond
	uc.budget = 30 * time.Second
	// Host scope for the boot sweep, through the REAL affinity seam the reconciler
	// uses (FleetVolumeManager over the same volume rows).
	uc.SetHostScope(selfHost, NewFleetVolumeManager(vols, &recordingBackend{}, "/vol"))

	return &restoreHarness{
		uc: uc, res: res, rp: rp, repo: repo, volumes: vols, replicas: replicas,
		scaler: scaler, health: health, store: store, live: live, dir: dir,
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
			name: "from pending is refused",
			mutate: func(h *restoreHarness) {
				_ = h.repo.UpdateResourcePhase(context.Background(), h.res.ID, domain.FleetResourcePhasePending)
				h.res.Phase = domain.FleetResourcePhasePending
			},
			wantErr:   domain.ErrRestoreInProgress,
			wantPhase: domain.FleetResourcePhasePending,
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
					ID: uuid.New(), AppID: appID, BackingPath: filepath.Join(h.dir, "vol2.ext4"),
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
	if len(rps) != 1 || !rps[0].Verified {
		t.Fatalf("recovery point must be marked verified, got %+v", rps)
	}
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
	if rps[0].Verified {
		t.Fatal("a recovery point that would not boot must NOT be marked verified")
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
		wantErr   bool
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
			name:    "no live and no anchor is an error, not a silent create",
			live:    "",
			pre:     "",
			wantErr: true,
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
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
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

	t.Run("refuses without an anchor", func(t *testing.T) {
		dir := t.TempDir()
		live := filepath.Join(dir, "vol.ext4")
		if err := os.WriteFile(live, []byte("RESTORED"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := swapBack(live, live+prerestoreSuffix, live+".failed-x"); err == nil {
			t.Fatal("want error when the pre-restore anchor is missing")
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
			ID: uuid.New(), AppID: appID, BackingPath: "/vol/other.ext4",
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
