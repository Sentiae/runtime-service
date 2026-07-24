package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

// newSnapshotHarness builds a snapshotter with fakes and a recording copyFile.
func newSnapshotHarness(t *testing.T, events *[]string) (*FleetVolumeSnapshotter, *fakePauser, *fakeArtifactStore, *fakeVolumeRepo, *fakeResourceRepo, *fakeResourceReplicaRepo) {
	t.Helper()
	pauser := &fakePauser{events: events}
	store := &fakeArtifactStore{events: events}
	vols := newFakeVolumeRepo()
	recovery := newFakeResourceRepo()
	replicas := newFakeResourceReplicaRepo()
	s := NewFleetVolumeSnapshotter(pauser, store, vols, replicas, recovery)
	s.copyFile = func(_, dst string) error {
		*events = append(*events, "copy")
		return os.WriteFile(dst, []byte("ext4-bytes"), 0o600)
	}
	return s, pauser, store, vols, recovery, replicas
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func TestSnapshot_AttachedPauseCopyResumeOrder(t *testing.T) {
	var events []string
	s, pauser, store, vols, recovery, replicas := newSnapshotHarness(t, &events)

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	if err := os.WriteFile(backing, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	appID := uuid.New()
	resID := uuid.New()
	replicaID := uuid.New()
	replicas.byID[replicaID] = &domain.Replica{ID: replicaID, SocketPath: "/tmp/x.sock"}
	volID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing, AttachedReplica: &replicaID}}

	points, err := s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := events; len(got) < 3 || got[0] != "pause" || got[1] != "copy" || got[2] != "resume" {
		t.Fatalf("order = %v, want pause,copy,resume,...", got)
	}
	if pauser.paused != 1 || pauser.resumed != 1 {
		t.Errorf("pause=%d resume=%d, want 1/1", pauser.paused, pauser.resumed)
	}
	if len(store.puts) != 1 {
		t.Fatalf("uploads = %d, want 1", len(store.puts))
	}
	wantKey := "volumes/" + volID.String() + "/" + points[0].ID.String() + ".ext4"
	if store.puts[0] != wantKey {
		t.Errorf("object key = %q, want %q", store.puts[0], wantKey)
	}
	// Recovery point recorded.
	rps, _ := recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 || rps[0].Kind != "snapshot" || rps[0].Verified {
		t.Errorf("recovery point = %+v", rps)
	}
	if rps[0].SizeBytes != int64(len("ext4-bytes")) {
		t.Errorf("size = %d, want %d", rps[0].SizeBytes, len("ext4-bytes"))
	}
	// SnapshotRef written on the volume.
	if got := vols.updated[volID].SnapshotRef; got != wantKey {
		t.Errorf("volume SnapshotRef = %q, want %q", got, wantKey)
	}
	// Temp file cleaned up.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file not cleaned: %d entries", len(entries))
	}
}

func TestSnapshot_ResumesOnCopyFailure(t *testing.T) {
	var events []string
	s, pauser, store, vols, recovery, replicas := newSnapshotHarness(t, &events)
	s.copyFile = func(_, _ string) error {
		events = append(events, "copy")
		return errors.New("disk full")
	}

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	replicaID := uuid.New()
	replicas.byID[replicaID] = &domain.Replica{ID: replicaID, SocketPath: "/tmp/x.sock"}
	volID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing, AttachedReplica: &replicaID}}

	_, err := s.SnapshotAppVolumes(context.Background(), uuid.New(), appID)
	if err == nil {
		t.Fatal("expected copy failure")
	}
	if pauser.resumed != 1 {
		t.Errorf("engine must be resumed even on copy failure (resumed=%d)", pauser.resumed)
	}
	if len(store.puts) != 0 {
		t.Errorf("nothing must be uploaded on copy failure")
	}
	rps, _ := recovery.ListRecoveryPoints(context.Background(), uuid.New())
	if len(rps) != 0 {
		t.Errorf("no recovery point on copy failure")
	}
}

func TestSnapshot_NoRowOnUploadFailure(t *testing.T) {
	var events []string
	s, pauser, store, vols, recovery, replicas := newSnapshotHarness(t, &events)
	store.putErr = errors.New("s3 down")

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	resID := uuid.New()
	replicaID := uuid.New()
	replicas.byID[replicaID] = &domain.Replica{ID: replicaID, SocketPath: "/tmp/x.sock"}
	volID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing, AttachedReplica: &replicaID}}

	_, err := s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err == nil {
		t.Fatal("expected upload failure")
	}
	if pauser.resumed != 1 {
		t.Errorf("engine must be resumed (resumed=%d)", pauser.resumed)
	}
	rps, _ := recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Errorf("a local-only copy is not a recovery point: got %d rows", len(rps))
	}
	if _, ok := vols.updated[volID]; ok {
		t.Errorf("SnapshotRef must not be written on upload failure")
	}
}

func TestSnapshot_UnattachedVolumeNoPause(t *testing.T) {
	var events []string
	s, pauser, store, vols, recovery, _ := newSnapshotHarness(t, &events)

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	resID := uuid.New()
	volID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing}} // AttachedReplica nil

	points, err := s.SnapshotAppVolumes(context.Background(), resID, appID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if pauser.paused != 0 || pauser.resumed != 0 {
		t.Errorf("unattached volume must not pause/resume (p=%d r=%d)", pauser.paused, pauser.resumed)
	}
	if len(store.puts) != 1 || len(points) != 1 {
		t.Errorf("unattached volume must still snapshot")
	}
	rps, _ := recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 1 {
		t.Errorf("recovery point missing for unattached volume")
	}
}

// D-184 — the recovery point records the sha256 of the uploaded bytes, computed
// on the SAME single pass as the upload. Until this landed a recovery point
// carried only its size, so a corrupt blob was indistinguishable from a good one.
func TestSnapshot_RecordsChecksumOfUploadedBytes(t *testing.T) {
	var events []string
	s, _, _, vols, recovery, _ := newSnapshotHarness(t, &events)

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	resID := uuid.New()
	volID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: volID, AppID: appID, BackingPath: backing}}

	if _, err := s.SnapshotAppVolumes(context.Background(), resID, appID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	rps, _ := recovery.ListRecoveryPoints(context.Background(), resID)
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
	s, _, store, vols, recovery, _ := newSnapshotHarness(t, &events)
	store.shortRead = true

	dir := t.TempDir()
	backing := filepath.Join(dir, "vol.ext4")
	_ = os.WriteFile(backing, []byte("data"), 0o600)
	appID := uuid.New()
	resID := uuid.New()
	vols.byApp[appID] = []domain.Volume{{ID: uuid.New(), AppID: appID, BackingPath: backing}}

	if _, err := s.SnapshotAppVolumes(context.Background(), resID, appID); err == nil {
		t.Fatal("want an error when the store did not consume the whole blob")
	}
	rps, _ := recovery.ListRecoveryPoints(context.Background(), resID)
	if len(rps) != 0 {
		t.Fatalf("no recovery point may be recorded, got %d", len(rps))
	}
}
