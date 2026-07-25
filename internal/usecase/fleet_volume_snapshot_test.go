package usecase

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes for the snapshotter
// ─────────────────────────────────────────────────────────────────────

// recorder collects the snapshotter's externally visible steps in order. It is
// mutex-guarded because the dead-man heartbeat records from its own goroutine
// while the upload records from the caller's.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) count(event string) int {
	n := 0
	for _, e := range r.all() {
		if e == event {
			n++
		}
	}
	return n
}

// fakeGuestControl records every control op in order and can fail any of them.
// Ordering is the point: a snapshot whose freeze lands AFTER the upload is
// exactly the torn-snapshot defect this slice closes.
type fakeGuestControl struct {
	rec *recorder

	mu        sync.Mutex
	syncErr   error
	freezeErr error
	// renewErr fails the dead-man heartbeat that holds the freeze open while the
	// upload runs, never the initial quiesce.
	renewErr error
	thawErr  error
	// thawBlocks makes Thaw hang until its context ends, modelling the real
	// client's long call timeout. It is how the retry loop is proven to retry.
	thawBlocks bool
	freezes    int
	renews     int
	thaws      int
}

func (f *fakeGuestControl) SyncFS(_ context.Context, _ string) error {
	f.rec.add("syncfs")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncErr
}

func (f *fakeGuestControl) Freeze(_ context.Context, _ string) error {
	f.rec.add("freeze")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes++
	return f.freezeErr
}

func (f *fakeGuestControl) Renew(_ context.Context, _ string) error {
	f.rec.add("renew")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	return f.renewErr
}

func (f *fakeGuestControl) Thaw(ctx context.Context, _ string) error {
	f.rec.add("thaw")
	f.mu.Lock()
	f.thaws++
	block, err := f.thawBlocks, f.thawErr
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return fmt.Errorf("guest control THAW: %w", ctx.Err())
	}
	return err
}

func (f *fakeGuestControl) Shutdown(_ context.Context, _ string) error { return nil }

func (f *fakeGuestControl) freezeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.freezes
}

func (f *fakeGuestControl) renewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renews
}

func (f *fakeGuestControl) thawCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.thaws
}

type fakeArtifactStore struct {
	mu        sync.Mutex
	rec       *recorder
	puts      []string
	putErr    error
	shortRead bool
	// readDelay slows every read of the incoming stream. The upload now runs
	// INSIDE the freeze, so a slow store is how the tests make the frozen window
	// long enough for the dead-man heartbeat to be observable.
	readDelay time.Duration
	// onPut runs when the store is handed the stream, before it consumes any of
	// it — the hook a test uses to interfere mid-upload (e.g. cancel the caller's
	// context) now that there is no staging copy to interfere with instead.
	onPut func()
	// bodies keeps the bytes AS STORED, so tests can assert the transfer size,
	// the checksum, and feed a stored blob straight back into the restore path.
	bodies map[string][]byte
}

func (f *fakeArtifactStore) Put(digest string, r io.Reader) error {
	f.rec.add("upload")
	f.mu.Lock()
	putErr, short, delay, hook := f.putErr, f.shortRead, f.readDelay, f.onPut
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if putErr != nil {
		return putErr
	}
	var body []byte
	if short {
		// Consume only the first byte — models a store that did not take the whole
		// blob, which must NOT yield a checksum of the full file.
		_, _ = io.CopyN(io.Discard, r, 1)
	} else {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, &slowReader{r: r, delay: delay}); err != nil {
			return err
		}
		body = buf.Bytes()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodies == nil {
		f.bodies = map[string][]byte{}
	}
	f.bodies[digest] = body
	f.puts = append(f.puts, digest)
	return nil
}

// slowReader paces a read loop so a test can hold the upload — and therefore the
// freeze — open for a controlled span.
type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.r.Read(p)
}

// putCount reports how many uploads the store COMPLETED.
func (f *fakeArtifactStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeArtifactStore) stored(digest string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[digest]
}
func (f *fakeArtifactStore) Get(string) (io.ReadCloser, error) { return nil, ErrArtifactNotFound }
func (f *fakeArtifactStore) Exists(string) (bool, error)       { return false, nil }
func (f *fakeArtifactStore) VerifyHash(string) error           { return nil }

type fakeVolumeRepo struct {
	mu      sync.Mutex
	byApp   map[uuid.UUID][]domain.Volume
	updated map[uuid.UUID]domain.Volume
}

func newFakeVolumeRepo() *fakeVolumeRepo {
	return &fakeVolumeRepo{byApp: map[uuid.UUID][]domain.Volume{}, updated: map[uuid.UUID]domain.Volume{}}
}

func (f *fakeVolumeRepo) Create(context.Context, *domain.Volume) error { return nil }
func (f *fakeVolumeRepo) ListByApp(_ context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	return f.byApp[appID], nil
}
func (f *fakeVolumeRepo) FindByID(context.Context, uuid.UUID) (*domain.Volume, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeVolumeRepo) Update(_ context.Context, v *domain.Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated[v.ID] = *v
	return nil
}
func (f *fakeVolumeRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *fakeVolumeRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Volume, error) {
	return nil, nil
}

// snapHarness bundles the snapshotter and every fake behind it.
type snapHarness struct {
	s        *FleetVolumeSnapshotter
	rec      *recorder
	guest    *fakeGuestControl
	store    *fakeArtifactStore
	vols     *fakeVolumeRepo
	recovery *fakeResourceRepo
	replicas *fakeResourceReplicaRepo
	// backing is the volume's real on-disk file. It IS the upload source now — no
	// staging copy stands between it and the store — so a test that wants
	// particular snapshot bytes writes them here.
	backing string
}

// volumeBytes is the backing-file content the snapshot tests expect to see land
// in the store.
const volumeBytes = "ext4-bytes"

// newSnapshotHarness builds a snapshotter with fakes behind it.
func newSnapshotHarness(t *testing.T) *snapHarness {
	t.Helper()
	rec := &recorder{}
	h := &snapHarness{
		rec:      rec,
		guest:    &fakeGuestControl{rec: rec},
		store:    &fakeArtifactStore{rec: rec},
		vols:     newFakeVolumeRepo(),
		recovery: newFakeResourceRepo(),
		replicas: newFakeResourceReplicaRepo(),
	}
	h.s = NewFleetVolumeSnapshotter(h.guest, h.store, h.vols, h.replicas, h.recovery)
	// Keep the thaw retry loop reachable without burning the real 10s deadline,
	// and park the heartbeat far out so only the tests that exercise it see one.
	h.s.thawDeadline = 60 * time.Millisecond
	h.s.thawRetry = 5 * time.Millisecond
	h.s.thawAttempt = 10 * time.Millisecond
	h.s.freezeHeartbeat = time.Hour
	return h
}

// attachedVolume seeds one app with a single backing file attached to a replica
// and returns the ids the tests assert on. dir is a REAL directory: the tests
// assert on what the snapshot does and does not create next to the volume.
func (h *snapHarness) attachedVolume(t *testing.T) (appID, resID, volID, replicaID uuid.UUID, dir string) {
	t.Helper()
	dir = t.TempDir()
	h.backing = filepath.Join(dir, "vol.ext4")
	if err := os.WriteFile(h.backing, []byte(volumeBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	appID, resID, volID, replicaID = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	h.replicas.byID[replicaID] = &domain.Replica{ID: replicaID, SocketPath: "/tmp/x.sock"}
	h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: h.backing, AttachedReplica: &replicaID}}
	return appID, resID, volID, replicaID, dir
}

// writeBacking replaces the volume's content.
func (h *snapHarness) writeBacking(t *testing.T, b []byte) {
	t.Helper()
	if err := os.WriteFile(h.backing, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// bigBacking fills the volume with n bytes of INCOMPRESSIBLE data so the upload
// is a genuine multi-write stream: that is what lets a test observe (and
// interrupt) an upload while it is in flight.
func (h *snapHarness) bigBacking(t *testing.T, n int) {
	t.Helper()
	buf := make([]byte, n)
	r := rand.New(rand.NewSource(1))
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	h.writeBacking(t, buf)
}

// assertNoLocalStaging is the whole point of the change: the snapshot must not
// materialize a second copy of the volume next to it. A host with less free space
// than the volume's size could not snapshot — and, since a durable resource
// refuses decommission without a final snapshot, could not free the space by
// deleting the resource either.
func assertNoLocalStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read volume dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".snap.tmp") {
			t.Fatalf("snapshot staged a local copy %q — the upload must stream the backing file itself", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("volume dir holds %d entries, want only the backing file: %v", len(entries), entries)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

// The ORDER is the contract, not the presence of the calls: the guest has to be
// flushed and frozen BEFORE the volume is read, and the thaw issued only AFTER
// the upload — the freeze is what makes streaming the live backing file safe, so
// it must cover the whole transfer. A test that merely counted the calls would
// still pass on the torn-snapshot behaviour this closes.
//
// It also pins the ABSENCE of a VMM pause/resume. Firecracker v1.16.0's vsock
// device does not survive Pause/Resume — the guest control channel is dead for
// the rest of the VM's life after the first pause — so reintroducing the pause
// (which looks like an obvious safety measure) breaks every subsequent snapshot
// of that VM. This is the assertion that catches it.
func TestSnapshot_AttachedQuiesceUploadThawOrder(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, _, dir := h.attachedVolume(t)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	events := h.rec.all()
	want := []string{"syncfs", "freeze", "upload", "thaw"}
	if !sameOrder(events, want) {
		t.Fatalf("order = %v, want %v", events, want)
	}
	for _, forbidden := range []string{"pause", "resume"} {
		if h.rec.count(forbidden) != 0 {
			t.Errorf("the attached path must never %s the VMM (fc v1.16.0 vsock does not survive it): %v", forbidden, events)
		}
	}
	if h.guest.thawCount() != 1 {
		t.Errorf("thaws = %d, want 1", h.guest.thawCount())
	}
	if len(h.store.puts) != 1 {
		t.Fatalf("uploads = %d, want 1", len(h.store.puts))
	}
	wantKey := "volumes/" + volID.String() + "/" + points[0].ID.String() + ".ext4"
	if h.store.puts[0] != wantKey {
		t.Errorf("object key = %q, want %q", h.store.puts[0], wantKey)
	}
	// Recovery point recorded.
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 || rps[0].Kind != "snapshot" || rps[0].RestoredInPlaceOK {
		t.Errorf("recovery point = %+v", rps)
	}
	if rps[0].SizeBytes != int64(len(volumeBytes)) {
		t.Errorf("size = %d, want %d", rps[0].SizeBytes, len(volumeBytes))
	}
	// SnapshotRef written on the volume.
	if got := h.vols.updated[volID].SnapshotRef; got != wantKey {
		t.Errorf("volume SnapshotRef = %q, want %q", got, wantKey)
	}
	// The upload streamed the backing file: nothing was staged beside it.
	assertNoLocalStaging(t, dir)
}

// With the vCPUs running, the dead-man the guest armed at Freeze keeps counting
// and would auto-thaw mid-transfer on any volume big enough to take longer than
// the window — and the transfer is now the UPLOAD, which is the slow part. The
// upload therefore RENEWs it on an interval; an upload that outlives the interval
// must show those renews — and they must be renews, not repeat freezes: FIFREEZE
// on an already-frozen filesystem returns EBUSY and re-arms nothing, so a
// heartbeat built on Freeze fails exactly when it is load-bearing.
func TestSnapshot_HeartbeatHoldsTheFreezeForALongUpload(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, dir := h.attachedVolume(t)
	h.bigBacking(t, 512<<10)
	h.s.freezeHeartbeat = 5 * time.Millisecond
	h.store.readDelay = 10 * time.Millisecond

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := h.guest.renewCount(); got < 2 {
		t.Fatalf("renews = %d, want at least 2 heartbeats across the upload", got)
	}
	if got := h.guest.freezeCount(); got != 1 {
		t.Fatalf("freezes = %d, want exactly the initial quiesce — a heartbeat must never re-FREEZE (EBUSY)", got)
	}
	events := h.rec.all()
	// The heartbeat runs CONCURRENTLY with the upload, so renews legitimately
	// interleave — asserting an exact prefix of syncfs,freeze,upload is a race
	// against how fast the first renew fires, not a statement about correctness.
	// Assert the invariant instead: quiesce first, upload strictly inside the
	// freeze window, renews spanning it.
	if !sameOrder(events[:2], []string{"syncfs", "freeze"}) {
		t.Fatalf("events = %v, want it to start syncfs,freeze", events)
	}
	uploadAt := -1
	for i, e := range events {
		if e == "upload" {
			if uploadAt != -1 {
				t.Fatalf("events = %v, want exactly one upload", events)
			}
			uploadAt = i
		}
	}
	if uploadAt < 2 {
		t.Fatalf("events = %v, want the upload after the quiesce", events)
	}
	if events[len(events)-1] != "thaw" {
		t.Fatalf("events = %v, want the thaw last — the freeze must outlive the upload", events)
	}
	// Everything between the upload and the thaw is a heartbeat renew. Indexed
	// from the upload's ACTUAL position, not a hardcoded one — a renew may fire
	// before the upload records, and that is the heartbeat working, not a fault.
	for _, e := range events[uploadAt+1 : len(events)-1] {
		if e != "renew" {
			t.Fatalf("events = %v, want only renew heartbeats between the upload and the thaw", events)
		}
	}
	assertNoLocalStaging(t, dir)
}

// A renew that fails means either the dead-man may fire at any moment or the
// freeze is already gone, so the upload cannot be trusted: it is aborted and the
// snapshot fails with no recovery point. "Continue and hope" would mint exactly
// the torn recovery point this path exists to prevent. The guest's refusal when
// no dead-man is armed (the freeze was lost) is the sharpest instance of it.
//
// The freeze now covers the whole upload, so this is the assertion that the
// upload itself is interruptible: the source reads are context-bound, so a dead
// heartbeat stops a transfer already in flight instead of letting it finish
// against a filesystem that may have thawed underneath it.
func TestSnapshot_FailedHeartbeatAbortsTheSnapshotMidUpload(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, _, dir := h.attachedVolume(t)
	// Big + slow: the upload is guaranteed to still be running when the first
	// heartbeat fails.
	h.bigBacking(t, 512<<10)
	h.store.readDelay = 5 * time.Millisecond
	h.s.freezeHeartbeat = 5 * time.Millisecond
	h.guest.renewErr = errors.New("guest refused RENEW: guest control: no dead-man armed — the freeze is gone")

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("want the failed re-arm to fail the snapshot")
	}
	if !strings.Contains(err.Error(), "re-arm guest dead-man") {
		t.Errorf("err = %v, want it to name the failed dead-man re-arm", err)
	}
	if h.rec.count("upload") == 0 {
		t.Fatal("the upload must have started — otherwise this proves nothing about aborting one in flight")
	}
	if got := h.store.putCount(); got != 0 {
		t.Errorf("completed uploads = %d, want 0 — the in-flight upload must be aborted, not left to finish", got)
	}
	assertNoLocalStaging(t, dir)
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("recovery points = %d, want 0 — a copy that may have raced the dead-man is not a recovery point", len(rps))
	}
	if _, ok := h.vols.updated[volID]; ok {
		t.Error("SnapshotRef must not be written when the heartbeat failed")
	}
	// The guest is still released: the freeze must not outlive the failed upload.
	if h.guest.thawCount() == 0 {
		t.Error("the guest must still be thawed after a heartbeat failure")
	}
}

// A guest that cannot be quiesced means a possibly-torn copy, and a torn copy
// recorded as a recovery point is trusted forever after — so the snapshot is
// refused outright, with nothing written anywhere.
func TestSnapshot_RefusesWhenTheGuestCannotBeQuiesced(t *testing.T) {
	controlDown := errors.New("vsock dial: connection refused")

	tests := []struct {
		name         string
		arm          func(*snapHarness)
		wantIs       []error
		wantMsgPart  string
		wantOrder    []string
		wantThawedGE int // thaws expected (freeze-lost-response window)
	}{
		{
			name:   "syncfs fails",
			arm:    func(h *snapHarness) { h.guest.syncErr = controlDown },
			wantIs: []error{domain.ErrSnapshotNotQuiescible, controlDown},
			// The channel IS armed for this VM, so the refusal must NOT send the
			// operator off to reboot it.
			wantMsgPart: "HAS a guest control channel",
			wantOrder:   []string{"syncfs"},
		},
		{
			name:         "freeze fails",
			arm:          func(h *snapHarness) { h.guest.freezeErr = controlDown },
			wantIs:       []error{domain.ErrSnapshotNotQuiescible, controlDown},
			wantMsgPart:  "HAS a guest control channel",
			wantOrder:    []string{"syncfs", "freeze", "thaw"},
			wantThawedGE: 1,
		},
		{
			name: "no control channel (VM predates it)",
			arm: func(h *snapHarness) {
				h.guest.syncErr = fmt.Errorf("guest control SYNCFS: %w", domain.ErrGuestControlUnavailable)
			},
			wantIs:      []error{domain.ErrSnapshotNotQuiescible, domain.ErrGuestControlUnavailable},
			wantMsgPart: "booted BEFORE the channel existed",
			wantOrder:   []string{"syncfs"},
		},
		{
			name:        "no control channel wired at all",
			arm:         func(h *snapHarness) { h.s.guest = nil },
			wantIs:      []error{domain.ErrSnapshotNotQuiescible, domain.ErrGuestControlUnavailable},
			wantMsgPart: "reboot the replica",
			wantOrder:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSnapshotHarness(t)
			appID, resID, volID, _, _ := h.attachedVolume(t)
			tt.arm(h)

			_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			for _, target := range tt.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("err = %v, want errors.Is %v", err, target)
				}
			}
			if tt.wantMsgPart != "" && !strings.Contains(err.Error(), tt.wantMsgPart) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tt.wantMsgPart)
			}
			if events := h.rec.all(); !sameOrder(events, tt.wantOrder) {
				t.Errorf("events = %v, want %v", events, tt.wantOrder)
			}
			if h.guest.thawCount() < tt.wantThawedGE {
				t.Errorf("thaws = %d, want >= %d", h.guest.thawCount(), tt.wantThawedGE)
			}
			// The volume is never read, uploaded or recorded.
			if h.rec.count("upload") != 0 {
				t.Errorf("upload attempts = %d, want 0", h.rec.count("upload"))
			}
			if len(h.store.puts) != 0 {
				t.Errorf("uploads = %d, want 0", len(h.store.puts))
			}
			rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
			if len(rps) != 0 {
				t.Errorf("recovery points = %d, want 0 — a snapshot that could be torn is not a recovery point", len(rps))
			}
			if _, ok := h.vols.updated[volID]; ok {
				t.Error("SnapshotRef must not be written for a refused snapshot")
			}
		})
	}
}

// The bytes are already in the store once the upload finished under the freeze,
// so a thaw that will not take must not fail the snapshot — and must NOT kill
// the replica either: the
// guest's own dead-man auto-thaw releases the filesystem within its window,
// while a kill would trade that bounded stall for a full database restart.
func TestSnapshot_ThawFailureKeepsTheSnapshotAndDoesNotKillTheReplica(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, replicaID, _ := h.attachedVolume(t)
	h.guest.thawErr = errors.New("guest refused THAW: device busy")

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("a thaw failure must not fail the snapshot: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("recovery points = %d, want 1", len(points))
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 {
		t.Errorf("recovery point rows = %d, want 1", len(rps))
	}
	if h.vols.updated[volID].SnapshotRef == "" {
		t.Error("SnapshotRef must still be written")
	}
	if h.guest.thawCount() < 2 {
		t.Errorf("thaw attempts = %d, want the retries to have run (>=2)", h.guest.thawCount())
	}
	// The replica survives: the snapshotter has no kill path at all any more, so
	// the recorded steps are exactly quiesce/upload/thaw-retries and the replica
	// row is untouched.
	events := h.rec.all()
	if !sameOrder(events[:3], []string{"syncfs", "freeze", "upload"}) {
		t.Fatalf("events = %v, want it to start syncfs,freeze,upload", events)
	}
	for _, e := range events[3:] {
		if e != "thaw" {
			t.Fatalf("events = %v: only thaw retries may run after the upload (no replica kill)", events)
		}
	}
	if _, err := h.replicas.FindByID(context.Background(), replicaID); err != nil {
		t.Errorf("the replica must be left running after a failed thaw: %v", err)
	}
}

// The thaw budget is 10s and one control call can block for 30s, so before this
// the "retries" ran exactly once and the deadline was already blown the first
// time it was checked. Each attempt now carries its own deadline.
func TestSnapshot_ThawRetriesReallyRetry(t *testing.T) {
	tests := []struct {
		name string
		arm  func(*fakeGuestControl)
	}{
		{
			name: "each attempt outlives its own deadline",
			arm:  func(g *fakeGuestControl) { g.thawBlocks = true },
		},
		{
			name: "each attempt is refused immediately",
			arm:  func(g *fakeGuestControl) { g.thawErr = errors.New("guest refused THAW: device busy") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSnapshotHarness(t)
			appID, resID, _, _, _ := h.attachedVolume(t)
			tt.arm(h.guest)

			started := time.Now()
			if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
				t.Fatalf("a thaw failure must not fail the snapshot: %v", err)
			}
			if got := h.guest.thawCount(); got < 2 {
				t.Fatalf("thaw attempts = %d, want more than one — the retry loop must be real", got)
			}
			// And the loop still respects its overall budget rather than running per
			// attempt: 60ms deadline, 10ms attempts, 5ms pauses.
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("thaw retries took %s, want them bounded by the deadline", elapsed)
			}
		})
	}
}

// A source the host cannot even open fails the snapshot — but not before the
// guest is released. The freeze is taken before the file is touched, so every
// exit from the capture, including this earliest one, still thaws.
func TestSnapshot_ThawsWhenTheBackingFileCannotBeRead(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	if err := os.Remove(h.backing); err != nil {
		t.Fatal(err)
	}

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected the unreadable backing file to fail the snapshot")
	}
	if h.guest.thawCount() != 1 {
		t.Errorf("guest must be thawed even when the source cannot be read (thaws=%d)", h.guest.thawCount())
	}
	if len(h.store.puts) != 0 {
		t.Errorf("nothing must be uploaded when the source cannot be read")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("no recovery point when the source cannot be read")
	}
}

// A backing file that is not on the host is a LEGIBLE condition, not an internal
// fault: it must carry ErrVolumeBackingFileMissing all the way out so the gRPC
// boundary can say why the teardown was refused instead of returning a bare
// Internal (#resource-final-snapshot-failure-is-a-bare-500). Both branches are
// pinned — attached and unattached reach os.Open by different routes.
func TestSnapshot_MissingBackingFileIsLegible(t *testing.T) {
	tests := []struct {
		name     string
		attached bool
	}{
		{name: "attached volume", attached: true},
		{name: "unattached volume", attached: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSnapshotHarness(t)
			appID, resID, volID, replicaID, _ := h.attachedVolume(t)
			if !tt.attached {
				h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: h.backing}}
				_ = replicaID
			}
			if err := os.Remove(h.backing); err != nil {
				t.Fatal(err)
			}

			_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
			if !errors.Is(err, domain.ErrVolumeBackingFileMissing) {
				t.Fatalf("got %v, want ErrVolumeBackingFileMissing", err)
			}
			// The refusal must stay a refusal: nothing stored, nothing recorded.
			if len(h.store.puts) != 0 {
				t.Errorf("nothing must be uploaded when the backing file is missing")
			}
			rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
			if len(rps) != 0 {
				t.Errorf("no recovery point may be invented: got %d", len(rps))
			}
		})
	}
}

func TestSnapshot_NoRowOnUploadFailure(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, _, _ := h.attachedVolume(t)
	h.store.putErr = errors.New("s3 down")

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected upload failure")
	}
	if h.guest.thawCount() != 1 {
		t.Errorf("guest must be thawed (thaws=%d)", h.guest.thawCount())
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("a local-only copy is not a recovery point: got %d rows", len(rps))
	}
	if _, ok := h.vols.updated[volID]; ok {
		t.Errorf("SnapshotRef must not be written on upload failure")
	}
}

func TestSnapshot_UnattachedVolumeNoQuiesce(t *testing.T) {
	h := newSnapshotHarness(t)

	dir := t.TempDir()
	h.backing = filepath.Join(dir, "vol.ext4")
	h.writeBacking(t, []byte(volumeBytes))
	appID := uuid.New()
	resID := uuid.New()
	volID := uuid.New()
	h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: h.backing}} // AttachedReplica nil

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// There is no guest to talk to when nothing is attached.
	if events := h.rec.all(); !sameOrder(events, []string{"upload"}) {
		t.Errorf("events = %v, want just the upload", events)
	}
	if len(h.store.puts) != 1 || len(points) != 1 {
		t.Errorf("unattached volume must still snapshot")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 {
		t.Errorf("recovery point missing for unattached volume")
	}
	// Detached streams too: no VM is writing the file, so a staging copy would
	// protect against nothing while still demanding the headroom that deadlocks
	// decommission. The consistency of this branch is bounded by how clean the
	// last engine STOP was (#resident-stop-is-vmm-kill) — staging never changed
	// that either way.
	assertNoLocalStaging(t, dir)
	if got := gunzip(t, h.store.stored(points[0].ObjectKey)); got != volumeBytes {
		t.Errorf("stored blob decompresses to %q, want the backing file itself", got)
	}
}

// sameOrder compares recorded events to the exact expected sequence.
func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// D-184 — the recovery point records the sha256 of the uploaded bytes, computed
// on the SAME single pass as the upload. Until this landed a recovery point
// carried only its size, so a corrupt blob was indistinguishable from a good one.
//
// The checksum covers the bytes AS STORED (the compressed stream): that is what
// "did the blob arrive intact" means, and it is what a restore can verify before
// materializing anything. size_bytes stays the LOGICAL volume size.
func TestSnapshot_RecordsChecksumOfUploadedBytes(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	body := h.store.stored(points[0].ObjectKey)
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); rps[0].Checksum != want {
		t.Fatalf("checksum = %q, want the sha256 of the STORED bytes %q", rps[0].Checksum, want)
	}
	if got := gunzip(t, body); got != volumeBytes { // the backing file, streamed as-is
		t.Fatalf("stored blob decompresses to %q, want the volume bytes", got)
	}
	if rps[0].SizeBytes != int64(len(volumeBytes)) {
		t.Fatalf("size_bytes = %d, want the LOGICAL volume size %d", rps[0].SizeBytes, len(volumeBytes))
	}
}

// The product-blocking defect: an upload reads a volume's HOLES back as zeros,
// so a 20GB-nominal volume holding a few GB transferred all 20GB and could not
// be snapshotted inside any sane deadline — and, because a durable resource
// refuses decommission without a final snapshot, could then never be deleted.
// Compression is what collapses those zero runs, so the transfer costs about
// what the real data costs.
func TestSnapshot_SparseVolumeStoresFarLessThanItsNominalSize(t *testing.T) {
	const (
		nominal  = 32 << 20 // logical volume size
		realData = 4 << 10  // the only bytes that are not a hole
	)
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	// A REAL sparse volume: a small write, then Truncate out to the nominal size.
	// Everything past the write is a hole that reads back as zeros.
	h.writeBacking(t, bytes.Repeat([]byte{0xAB}, realData))
	if err := os.Truncate(h.backing, nominal); err != nil {
		t.Fatal(err)
	}

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	stored := int64(len(h.store.stored(points[0].ObjectKey)))
	if points[0].SizeBytes != nominal {
		t.Fatalf("size_bytes = %d, want the LOGICAL %d", points[0].SizeBytes, nominal)
	}
	// The bar is deliberately loose (a 100:1 floor against deflate's ~1000:1 on
	// zeros): the assertion is "holes cost nothing", not a compression ratio.
	if stored >= nominal/100 {
		t.Fatalf("stored %d bytes for a %d-byte volume holding %d bytes of data — holes are still being transferred",
			stored, int64(nominal), int64(realData))
	}
}

// gunzip decompresses a stored snapshot blob for assertions.
func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

// A store that consumes only part of the blob must NOT produce a recovery point:
// the digest would describe bytes nobody stored, and a later restore would
// "verify" against it.
func TestSnapshot_RefusesWhenStoreDidNotConsumeWholeBlob(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.store.shortRead = true

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("want an error when the store did not consume the whole blob")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Fatalf("no recovery point may be recorded, got %d", len(rps))
	}
}

// limitStore models an artifact store that stops reading early: `limit` bytes
// then a SUCCESSFUL return. That is the dangerous shape — a failure the caller
// would otherwise record a checksum over.
type limitStore struct {
	limit int64 // <0 consumes everything
	err   error
	body  bytes.Buffer
}

func (s *limitStore) Put(_ string, r io.Reader) error {
	if s.err != nil {
		return s.err
	}
	if s.limit < 0 {
		_, _ = io.Copy(&s.body, r)
		return nil
	}
	_, _ = io.CopyN(&s.body, r, s.limit)
	return nil
}
func (s *limitStore) Get(string) (io.ReadCloser, error) { return nil, ErrArtifactNotFound }
func (s *limitStore) Exists(string) (bool, error)       { return false, nil }
func (s *limitStore) VerifyHash(string) error           { return nil }

// The upload's honesty gate. Compression means "bytes stored" and "bytes of the
// volume" are different numbers, so the guard compares two pairs: the whole
// source must have been read (else the stream is a valid gzip of a PREFIX of
// the volume — a truncation no size check on the stored blob can see any more),
// and every compressed byte produced must have been consumed by the store (else
// the digest describes bytes nobody stored).
func TestUploadSnapshotFileHashed_TruncationGuard(t *testing.T) {
	payload := bytes.Repeat([]byte("volume-bytes"), 512)

	tests := []struct {
		name    string
		store   *limitStore
		wantErr bool
	}{
		{"store consumes the whole compressed stream", &limitStore{limit: -1}, false},
		{"store consumes a prefix and reports success", &limitStore{limit: 4}, true},
		{"store reads nothing and reports success", &limitStore{limit: 0}, true},
		{"store fails outright", &limitStore{limit: -1, err: errors.New("s3 down")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snap.tmp")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			up, err := uploadSnapshotFileHashed(context.Background(), tt.store, "k", path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got upload %+v", up)
				}
				return
			}
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			if up.LogicalBytes != int64(len(payload)) {
				t.Errorf("logical = %d, want %d", up.LogicalBytes, len(payload))
			}
			if up.StoredBytes != int64(tt.store.body.Len()) {
				t.Errorf("stored = %d, store took %d", up.StoredBytes, tt.store.body.Len())
			}
			sum := sha256.Sum256(tt.store.body.Bytes())
			if want := hex.EncodeToString(sum[:]); up.Checksum != want {
				t.Errorf("checksum = %q, want the sha256 of the stored bytes %q", up.Checksum, want)
			}
		})
	}
}

// Every abort path must leave zero descriptors behind. The snapshot now holds an
// open descriptor on the LIVE backing file for the whole upload, so a leak here
// pins a customer volume's inode (and, when the volume is later deleted, its disk
// until the process dies — measured live as ~28GB of held-open deleted files
// after repeated cancels).
//
// The proxy is the fd NUMBER a fresh open gets: on Unix the kernel hands out the
// lowest free descriptor, so leaked descriptors push it up. It proves "no
// descriptor outlived these snapshots"; it does not attribute a leak to a
// particular file, and it cannot see a descriptor another test leaked.
func TestSnapshot_AbortedSnapshotLeavesNoOpenDescriptor(t *testing.T) {
	aborts := []struct {
		name  string
		setup func(t *testing.T, h *snapHarness) context.Context
	}{
		{
			name: "context cancelled mid-upload",
			setup: func(t *testing.T, h *snapHarness) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				// Big enough that the store is handed the stream long before the
				// compressor has read the file out: cancelling from the store's own
				// hook therefore lands in the MIDDLE of the transfer.
				h.bigBacking(t, 512<<10)
				h.store.onPut = cancel
				return ctx
			},
		},
		{
			name: "upload fails",
			setup: func(_ *testing.T, h *snapHarness) context.Context {
				h.store.putErr = errors.New("s3 down")
				return context.Background()
			},
		},
		{
			name: "store stops reading mid-upload",
			setup: func(_ *testing.T, h *snapHarness) context.Context {
				h.store.shortRead = true
				return context.Background()
			},
		},
	}

	const rounds = 4
	baseFD := probeFD(t)
	baseGoroutines := runtime.NumGoroutine()

	for i := 0; i < rounds; i++ {
		for _, ab := range aborts {
			h := newSnapshotHarness(t)
			appID, resID, _, _, dir := h.attachedVolume(t)
			ctx := ab.setup(t, h)
			if _, err := h.s.SnapshotAppVolumes(ctx, resID, appID); err == nil {
				t.Fatalf("%s: expected the snapshot to abort", ab.name)
			}
			// An aborted snapshot leaves nothing behind either — least of all a
			// half-written copy of the volume.
			assertNoLocalStaging(t, dir)
		}
	}

	if got := probeFD(t); got > baseFD+2 {
		t.Fatalf("a fresh open landed on fd %d, was %d before %d aborted snapshots — descriptors outlived them",
			got, baseFD, rounds*len(aborts))
	}
	// A compressor goroutine parked on the upload pipe would hold the file it is
	// reading, so it is joined on every exit. Settle briefly: unrelated tests in
	// this package run background goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseGoroutines+4 {
		t.Fatalf("goroutines = %d, was %d before the aborted snapshots — an upload goroutine outlived its snapshot", got, baseGoroutines)
	}
}

// probeFD reports the descriptor number a fresh open receives.
func probeFD(t *testing.T) uintptr {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	fd := f.Fd()
	if err := f.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
	return fd
}

// Every recovery point must record HOW it was made consistent — it is
// unbackfillable (nothing about a blob in the store reveals whether the
// filesystem it was read from was frozen) and PITR needs it to know which anchors
// are valid.
//
// The attached path holds the guest freeze for the WHOLE upload, so what it
// produces is a crash-free filesystem image: guest_frozen.
func TestSnapshot_AttachedCaptureStampsGuestFrozen(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := points[0].Consistency; got != domain.RecoveryPointGuestFrozen {
		t.Errorf("returned consistency = %q, want %q", got, domain.RecoveryPointGuestFrozen)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if got := rps[0].Consistency; got != domain.RecoveryPointGuestFrozen {
		t.Errorf("recorded consistency = %q, want %q", got, domain.RecoveryPointGuestFrozen)
	}
}

// The detached path has no freeze AND cannot prove the writer stopped cleanly (a
// resident stop is a VMM kill, #resident-stop-is-vmm-kill), so it is
// detached_unclean — a crashed filesystem image, not a PITR anchor. Stamping
// detached_clean here would be the optimistic label the class exists to prevent.
func TestSnapshot_DetachedCaptureStampsDetachedUnclean(t *testing.T) {
	h := newSnapshotHarness(t)

	dir := t.TempDir()
	h.backing = filepath.Join(dir, "vol.ext4")
	h.writeBacking(t, []byte(volumeBytes))
	appID, resID, volID := uuid.New(), uuid.New(), uuid.New()
	h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: h.backing}} // AttachedReplica nil

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := points[0].Consistency; got != domain.RecoveryPointDetachedUnclean {
		t.Errorf("returned consistency = %q, want %q", got, domain.RecoveryPointDetachedUnclean)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if got := rps[0].Consistency; got != domain.RecoveryPointDetachedUnclean {
		t.Errorf("recorded consistency = %q, want %q", got, domain.RecoveryPointDetachedUnclean)
	}
	if got := rps[0].Consistency; got == domain.RecoveryPointGuestFrozen {
		t.Errorf("a capture with no freeze must never be stamped %q", got)
	}
}

// A capture that ABORTS must leave no row at all — so there is never a recovery
// point stamped with a class its bytes do not have. The freeze lapsing mid-upload
// (a failed dead-man renew) is exactly the case that would otherwise mint a torn
// artifact labelled guest_frozen.
func TestSnapshot_AbortedFreezeLeavesNoStampedRecoveryPoint(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.bigBacking(t, 4<<20)
	h.s.freezeHeartbeat = time.Millisecond
	h.guest.mu.Lock()
	h.guest.renewErr = errors.New("dead-man re-arm refused")
	h.guest.mu.Unlock()

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("a lapsed freeze must fail the snapshot")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Fatalf("aborted capture recorded %d recovery point(s): %+v", len(rps), rps)
	}
}
