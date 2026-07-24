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

type fakePauser struct {
	events    *[]string
	pauseErr  error
	resumeErr error
	paused    int
	resumed   int
}

func (f *fakePauser) Pause(_ context.Context, _ string) error {
	*f.events = append(*f.events, "pause")
	f.paused++
	return f.pauseErr
}
func (f *fakePauser) Resume(_ context.Context, _ string) error {
	*f.events = append(*f.events, "resume")
	f.resumed++
	return f.resumeErr
}

// fakeGuestControl records every control op in order and can fail any of them.
// Ordering is the point: a snapshot whose freeze lands AFTER the copy is exactly
// the torn-snapshot defect this slice closes.
type fakeGuestControl struct {
	events    *[]string
	syncErr   error
	freezeErr error
	thawErr   error
	thaws     int
}

func (f *fakeGuestControl) SyncFS(_ context.Context, _ string) error {
	*f.events = append(*f.events, "syncfs")
	return f.syncErr
}
func (f *fakeGuestControl) Freeze(_ context.Context, _ string) error {
	*f.events = append(*f.events, "freeze")
	return f.freezeErr
}
func (f *fakeGuestControl) Thaw(_ context.Context, _ string) error {
	*f.events = append(*f.events, "thaw")
	f.thaws++
	return f.thawErr
}
func (f *fakeGuestControl) Shutdown(_ context.Context, _ string) error { return nil }

// fakeReplicaRecycler records the escalation kill.
type fakeReplicaRecycler struct {
	events *[]string
	killed []uuid.UUID
	err    error
}

func (f *fakeReplicaRecycler) DecommissionReplica(_ context.Context, id uuid.UUID) error {
	*f.events = append(*f.events, "kill-replica")
	f.killed = append(f.killed, id)
	return f.err
}

type fakeArtifactStore struct {
	mu        sync.Mutex
	events    *[]string
	puts      []string
	putErr    error
	shortRead bool
}

func (f *fakeArtifactStore) Put(digest string, r io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.events = append(*f.events, "upload")
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
	pauser   *fakePauser
	guest    *fakeGuestControl
	recycler *fakeReplicaRecycler
	store    *fakeArtifactStore
	vols     *fakeVolumeRepo
	recovery *fakeResourceRepo
	replicas *fakeResourceReplicaRepo
}

// newSnapshotHarness builds a snapshotter with fakes and a recording copyFile.
func newSnapshotHarness(t *testing.T, events *[]string) *snapHarness {
	t.Helper()
	h := &snapHarness{
		pauser:   &fakePauser{events: events},
		guest:    &fakeGuestControl{events: events},
		recycler: &fakeReplicaRecycler{events: events},
		store:    &fakeArtifactStore{events: events},
		vols:     newFakeVolumeRepo(),
		recovery: newFakeResourceRepo(),
		replicas: newFakeResourceReplicaRepo(),
	}
	h.s = NewFleetVolumeSnapshotter(h.pauser, h.guest, h.recycler, h.store, h.vols, h.replicas, h.recovery)
	h.s.copyFile = func(_, dst string) error {
		*events = append(*events, "copy")
		return os.WriteFile(dst, []byte("ext4-bytes"), 0o600)
	}
	// Keep the thaw escalation reachable without burning the real 10s deadline.
	h.s.thawDeadline = 20 * time.Millisecond
	h.s.thawRetry = 5 * time.Millisecond
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
// flushed and frozen BEFORE the VMM pause, the host fd flushed and the copy taken
// while both hold, and the thaw issued only after the resume. A test that merely
// counted the calls would still pass on the torn-snapshot behaviour this closes.
func TestSnapshot_AttachedQuiescePauseCopyResumeThawOrder(t *testing.T) {
	var events []string
	h := newSnapshotHarness(t, &events)
	appID, resID, volID, _, dir := h.attachedVolume(t)

	points, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	want := []string{"syncfs", "freeze", "pause", "copy", "resume", "thaw", "upload"}
	if !sameOrder(events, want) {
		t.Fatalf("order = %v, want %v", events, want)
	}
	if h.pauser.paused != 1 || h.pauser.resumed != 1 {
		t.Errorf("pause=%d resume=%d, want 1/1", h.pauser.paused, h.pauser.resumed)
	}
	if h.guest.thaws != 1 {
		t.Errorf("thaws = %d, want 1", h.guest.thaws)
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
			name:      "syncfs fails",
			arm:       func(h *snapHarness) { h.guest.syncErr = controlDown },
			wantIs:    []error{domain.ErrSnapshotNotQuiescible, controlDown},
			wantOrder: []string{"syncfs"},
		},
		{
			name:         "freeze fails",
			arm:          func(h *snapHarness) { h.guest.freezeErr = controlDown },
			wantIs:       []error{domain.ErrSnapshotNotQuiescible, controlDown},
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
			var events []string
			h := newSnapshotHarness(t, &events)
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
			if !sameOrder(events, tt.wantOrder) {
				t.Errorf("events = %v, want %v", events, tt.wantOrder)
			}
			if h.guest.thaws < tt.wantThawedGE {
				t.Errorf("thaws = %d, want >= %d", h.guest.thaws, tt.wantThawedGE)
			}
			// The VM is never paused and nothing is copied, uploaded or recorded.
			if h.pauser.paused != 0 {
				t.Errorf("pauses = %d, want 0", h.pauser.paused)
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
// take must not fail the snapshot — but it must not leave writers blocked
// either: the replica is killed and the reconciler boots a replacement.
func TestSnapshot_ThawFailureKeepsTheSnapshotAndEscalates(t *testing.T) {
	var events []string
	h := newSnapshotHarness(t, &events)
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
	if h.guest.thaws < 2 {
		t.Errorf("thaw attempts = %d, want the retries to have run (>=2)", h.guest.thaws)
	}
	if len(h.recycler.killed) != 1 || h.recycler.killed[0] != replicaID {
		t.Fatalf("escalation did not kill the wedged replica: killed = %v", h.recycler.killed)
	}
	// The retries make the thaw count vary with timing, so assert the fixed head
	// and tail: the kill lands after the retries and before the upload — the guest
	// is unblocked first.
	if !sameOrder(events[:6], []string{"syncfs", "freeze", "pause", "copy", "resume", "thaw"}) {
		t.Errorf("order = %v", events)
	}
	if !sameOrder(events[len(events)-2:], []string{"kill-replica", "upload"}) {
		t.Errorf("order = %v, want it to end kill-replica,upload", events)
	}
}

// A pause that fails after the freeze took must still thaw: the copy never
// happens, and the guest cannot be left frozen because of it.
func TestSnapshot_ThawsWhenThePauseFails(t *testing.T) {
	var events []string
	h := newSnapshotHarness(t, &events)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.pauser.pauseErr = errors.New("firecracker api: 500")

	if _, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("want the pause failure to abort the snapshot")
	}
	if !sameOrder(events, []string{"syncfs", "freeze", "pause", "thaw"}) {
		t.Fatalf("order = %v, want syncfs,freeze,pause,thaw", events)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("recovery points = %d, want 0", len(rps))
	}
}

func TestSnapshot_ResumesAndThawsOnCopyFailure(t *testing.T) {
	var events []string
	h := newSnapshotHarness(t, &events)
	appID, resID, _, _, _ := h.attachedVolume(t)
	h.s.copyFile = func(_, _ string) error {
		events = append(events, "copy")
		return errors.New("disk full")
	}

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected copy failure")
	}
	if h.pauser.resumed != 1 {
		t.Errorf("engine must be resumed even on copy failure (resumed=%d)", h.pauser.resumed)
	}
	if h.guest.thaws != 1 {
		t.Errorf("guest must be thawed even on copy failure (thaws=%d)", h.guest.thaws)
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
	var events []string
	h := newSnapshotHarness(t, &events)
	appID, resID, volID, _, _ := h.attachedVolume(t)
	h.store.putErr = errors.New("s3 down")

	_, err := h.s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected upload failure")
	}
	if h.pauser.resumed != 1 {
		t.Errorf("engine must be resumed (resumed=%d)", h.pauser.resumed)
	}
	if h.guest.thaws != 1 {
		t.Errorf("guest must be thawed (thaws=%d)", h.guest.thaws)
	}
	rps, _ := h.recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("a local-only copy is not a recovery point: got %d rows", len(rps))
	}
	if _, ok := h.vols.updated[volID]; ok {
		t.Errorf("SnapshotRef must not be written on upload failure")
	}
}

func TestSnapshot_UnattachedVolumeNoPauseNoQuiesce(t *testing.T) {
	var events []string
	h := newSnapshotHarness(t, &events)

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
	if h.pauser.paused != 0 || h.pauser.resumed != 0 {
		t.Errorf("unattached volume must not pause/resume (p=%d r=%d)", h.pauser.paused, h.pauser.resumed)
	}
	// There is no guest to talk to when nothing is attached.
	if !sameOrder(events, []string{"copy", "upload"}) {
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
	var events []string
	h := newSnapshotHarness(t, &events)
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
	var events []string
	h := newSnapshotHarness(t, &events)
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
