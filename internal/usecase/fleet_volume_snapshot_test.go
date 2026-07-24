package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
// Fakes for the snapshotter
// ─────────────────────────────────────────────────────────────────────

// recorder collects the snapshotter's externally visible steps in order. It is
// mutex-guarded because the dead-man heartbeat records from its own goroutine
// while the copy records from the caller's.
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
// Ordering is the point: a snapshot whose freeze lands AFTER the copy is exactly
// the torn-snapshot defect this slice closes.
type fakeGuestControl struct {
	rec *recorder

	mu        sync.Mutex
	syncErr   error
	freezeErr error
	// renewErr fails the dead-man heartbeat that holds the freeze open while the
	// copy runs, never the initial quiesce.
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
}

func (f *fakeArtifactStore) Put(digest string, r io.Reader) error {
	f.rec.add("upload")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	if f.shortRead {
		// Consume only the first byte — models a store that did not take the whole
		// blob, which must NOT yield a checksum of the full file.
		_, _ = io.CopyN(io.Discard, r, 1)
	} else {
		_, _ = io.Copy(io.Discard, r)
	}
	f.puts = append(f.puts, digest)
	return nil
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
}

// newSnapshotHarness builds a snapshotter with fakes and a recording copyFile.
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
	h.s.copyFile = func(_ context.Context, _, dst string) error {
		rec.add("copy")
		return os.WriteFile(dst, []byte("ext4-bytes"), 0o600)
	}
	// Keep the thaw retry loop reachable without burning the real 10s deadline,
	// and park the heartbeat far out so only the tests that exercise it see one.
	h.s.thawDeadline = 60 * time.Millisecond
	h.s.thawRetry = 5 * time.Millisecond
	h.s.thawAttempt = 10 * time.Millisecond
	h.s.freezeHeartbeat = time.Hour
	return h
}

// attachedVolume seeds one app with a single backing file attached to a replica
// and returns the ids the tests assert on.
func (h *snapHarness) attachedVolume(t *testing.T) (appID, resID, volID, replicaID uuid.UUID, dir string) {
	t.Helper()
	dir = t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	if err := os.WriteFile(backing, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	appID, resID, volID, replicaID = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	h.replicas.byID[replicaID] = &domain.Replica{ID: replicaID, SocketPath: "/tmp/x.sock"}
	h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing, AttachedReplica: &replicaID}}
	return appID, resID, volID, replicaID, dir
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

// The ORDER is the contract, not the presence of the calls: the guest has to be
// flushed and frozen BEFORE the copy, and the thaw issued only after it. A test
// that merely counted the calls would still pass on the torn-snapshot behaviour
// this closes.
//
// It also pins the ABSENCE of a VMM pause/resume. Firecracker v1.16.0's vsock
// device does not survive Pause/Resume — the guest control channel is dead for
// the rest of the VM's life after the first pause — so reintroducing the pause
// (which looks like an obvious safety measure) breaks every subsequent snapshot
// of that VM. This is the assertion that catches it.
func TestSnapshot_AttachedQuiesceCopyThawOrder(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, _, dir := h.attachedVolume(t)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	events := h.rec.all()
	want := []string{"syncfs", "freeze", "copy", "thaw", "upload"}
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
	if len(rps) != 1 || rps[0].Kind != "snapshot" || rps[0].Verified {
		t.Errorf("recovery point = %+v", rps)
	}
	if rps[0].SizeBytes != int64(len("ext4-bytes")) {
		t.Errorf("size = %d, want %d", rps[0].SizeBytes, len("ext4-bytes"))
	}
	// SnapshotRef written on the volume.
	if got := h.vols.updated[volID].SnapshotRef; got != wantKey {
		t.Errorf("volume SnapshotRef = %q, want %q", got, wantKey)
	}
	// Temp file cleaned up.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file not cleaned: %d entries", len(entries))
	}
}

// With the vCPUs running, the dead-man the guest armed at Freeze keeps counting
// and would auto-thaw mid-copy on any volume big enough to take longer than the
// window. The copy therefore RENEWs it on an interval; a copy that outlives the
// interval must show those renews — and they must be renews, not repeat freezes:
// FIFREEZE on an already-frozen filesystem returns EBUSY and re-arms nothing, so
// a heartbeat built on Freeze fails exactly when it is load-bearing.
func TestSnapshot_HeartbeatHoldsTheFreezeForALongCopy(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.s.freezeHeartbeat = 10 * time.Millisecond
	h.s.copyFile = func(ctx context.Context, _, dst string) error {
		h.rec.add("copy")
		select {
		case <-time.After(80 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		return os.WriteFile(dst, []byte("ext4-bytes"), 0o600)
	}

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := h.guest.renewCount(); got < 2 {
		t.Fatalf("renews = %d, want at least 2 heartbeats across the copy", got)
	}
	if got := h.guest.freezeCount(); got != 1 {
		t.Fatalf("freezes = %d, want exactly the initial quiesce — a heartbeat must never re-FREEZE (EBUSY)", got)
	}
	events := h.rec.all()
	if !sameOrder(events[:3], []string{"syncfs", "freeze", "copy"}) {
		t.Fatalf("events = %v, want it to start syncfs,freeze,copy", events)
	}
	if !sameOrder(events[len(events)-2:], []string{"thaw", "upload"}) {
		t.Fatalf("events = %v, want it to end thaw,upload", events)
	}
	// Everything between the copy and the thaw is a heartbeat renew.
	for _, e := range events[3 : len(events)-2] {
		if e != "renew" {
			t.Fatalf("events = %v, want only renew heartbeats between copy and thaw", events)
		}
	}
}

// A renew that fails means either the dead-man may fire at any moment or the
// freeze is already gone, so the copy cannot be trusted: it is aborted and the
// snapshot fails with no recovery point. "Continue and hope" would mint exactly
// the torn recovery point this path exists to prevent. The guest's refusal when
// no dead-man is armed (the freeze was lost) is the sharpest instance of it.
func TestSnapshot_FailedHeartbeatAbortsTheSnapshot(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, volID, _, _ := h.attachedVolume(t)
	h.s.freezeHeartbeat = 5 * time.Millisecond
	h.guest.renewErr = errors.New("guest refused RENEW: guest control: no dead-man armed — the freeze is gone")
	copyAborted := make(chan struct{})
	h.s.copyFile = func(ctx context.Context, _, _ string) error {
		h.rec.add("copy")
		// Never completes on its own: the only way out is the heartbeat aborting it,
		// which is the behaviour under test.
		<-ctx.Done()
		close(copyAborted)
		return ctx.Err()
	}

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("want the failed re-arm to fail the snapshot")
	}
	if !strings.Contains(err.Error(), "re-arm guest dead-man") {
		t.Errorf("err = %v, want it to name the failed dead-man re-arm", err)
	}
	select {
	case <-copyAborted:
	default:
		t.Error("the in-flight copy must be aborted, not left to finish")
	}
	if len(h.store.puts) != 0 {
		t.Errorf("uploads = %d, want 0", len(h.store.puts))
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("recovery points = %d, want 0 — a copy that may have raced the dead-man is not a recovery point", len(rps))
	}
	if _, ok := h.vols.updated[volID]; ok {
		t.Error("SnapshotRef must not be written when the heartbeat failed")
	}
	// The guest is still released: the freeze must not outlive the failed copy.
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
			// Nothing is copied, uploaded or recorded.
			if h.rec.count("copy") != 0 {
				t.Errorf("copies = %d, want 0", h.rec.count("copy"))
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

// The copy is already consistent once the freeze held, so a thaw that will not
// take must not fail the snapshot — and must NOT kill the replica either: the
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
	// the recorded steps are exactly quiesce/copy/thaw/upload and the replica row
	// is untouched.
	events := h.rec.all()
	if !sameOrder(events[:3], []string{"syncfs", "freeze", "copy"}) {
		t.Fatalf("events = %v, want it to start syncfs,freeze,copy", events)
	}
	if events[len(events)-1] != "upload" {
		t.Fatalf("events = %v, want it to end with the upload", events)
	}
	for _, e := range events[3 : len(events)-1] {
		if e != "thaw" {
			t.Fatalf("events = %v: only thaw retries may run between the copy and the upload (no replica kill)", events)
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

func TestSnapshot_ThawsOnCopyFailure(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.s.copyFile = func(context.Context, string, string) error {
		h.rec.add("copy")
		return errors.New("disk full")
	}

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected copy failure")
	}
	if h.guest.thawCount() != 1 {
		t.Errorf("guest must be thawed even on copy failure (thaws=%d)", h.guest.thawCount())
	}
	if len(h.store.puts) != 0 {
		t.Errorf("nothing must be uploaded on copy failure")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("no recovery point on copy failure")
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
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	resID := uuid.New()
	volID := uuid.New()
	h.vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing}} // AttachedReplica nil

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// There is no guest to talk to when nothing is attached.
	if events := h.rec.all(); !sameOrder(events, []string{"copy", "upload"}) {
		t.Errorf("events = %v, want copy,upload", events)
	}
	if len(h.store.puts) != 1 || len(points) != 1 {
		t.Errorf("unattached volume must still snapshot")
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 {
		t.Errorf("recovery point missing for unattached volume")
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
func TestSnapshot_RecordsChecksumOfUploadedBytes(t *testing.T) {
	h := newSnapshotHarness(t)
	appID, resID, _, _, _ := h.attachedVolume(t)

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	sum := sha256.Sum256([]byte("ext4-bytes")) // what the harness copyFile writes
	if want := hex.EncodeToString(sum[:]); rps[0].Checksum != want {
		t.Fatalf("checksum = %q, want %q", rps[0].Checksum, want)
	}
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
