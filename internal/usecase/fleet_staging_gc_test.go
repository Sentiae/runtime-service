package usecase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// stale makes a staging directory look untouched for longer than the grace
// period, which is what the sweep requires before it will consider one.
func stale(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * stagingOrphanGrace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// stagingDir materializes what a real boot leaves under the work root: the
// per-workload directory plus a rootfs image inside it.
func stagingDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	stale(t, dir)
	return dir
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return false
}

// errReplicaRepo makes the replica lookup fail, standing in for a DB outage.
type errReplicaRepo struct{ *rtReplicaRepo }

func (errReplicaRepo) FindByID(context.Context, uuid.UUID) (*domain.Replica, error) {
	return nil, errors.New("connection refused")
}

// TestStagingSweep_ReclaimsOnlyProvableOrphans is the
// #fleet-image-staging-dirs-no-gc regression AND its safety proof: the sweep
// must reclaim a directory whose owner is gone, and must leave every directory
// it cannot POSITIVELY prove is an orphan — removing a live VM's rootfs would be
// far worse than the leak.
func TestStagingSweep_ReclaimsOnlyProvableOrphans(t *testing.T) {
	liveReplicaID := uuid.New()
	liveWorkloadID := uuid.New()
	orphanID := uuid.New()
	freshOrphanID := uuid.New()

	root := t.TempDir()
	replicas := newRTReplicaRepo()
	_ = replicas.Create(context.Background(), &domain.Replica{ID: liveReplicaID, State: domain.ReplicaStateResident})
	workloads := newFakeWorkloadRepo()
	_ = workloads.Create(context.Background(), &domain.ImageWorkload{ID: liveWorkloadID, State: domain.ImageWorkloadStateRunning})

	orphan := stagingDir(t, root, orphanID.String())
	liveReplica := stagingDir(t, root, liveReplicaID.String())
	liveWorkload := stagingDir(t, root, liveWorkloadID.String())
	notAUUID := stagingDir(t, root, "warm-templates")
	// A directory named like a uuid but created moments ago: inside the grace
	// period, so it is left alone this pass.
	freshOrphan := filepath.Join(root, freshOrphanID.String())
	if err := os.MkdirAll(freshOrphan, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A plain FILE whose name parses as a uuid is not a staging dir.
	uuidFile := filepath.Join(root, uuid.NewString())
	if err := os.WriteFile(uuidFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stale(t, uuidFile)

	uc := NewFleetStagingSweeper(replicas, workloads, root)
	out, err := uc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if out.Reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want exactly 1 (the orphan)", out.Reclaimed)
	}
	if out.Bytes < 2048 {
		t.Fatalf("bytes = %d, want at least the 2048-byte rootfs", out.Bytes)
	}
	if dirExists(t, orphan) {
		t.Fatalf("the orphaned staging dir %s should have been reclaimed", orphan)
	}
	for what, path := range map[string]string{
		"a live replica's rootfs":              liveReplica,
		"a live image workload's rootfs":       liveWorkload,
		"a directory that is not a uuid":       notAUUID,
		"a directory inside the grace period":  freshOrphan,
		"a file that happens to be uuid-named": uuidFile,
	} {
		if !dirExists(t, path) {
			t.Fatalf("%s (%s) must never be removed", what, path)
		}
	}
}

// TestStagingSweep_LookupFailureKeepsTheDirectory: a repository that cannot
// answer means "might be live". The sweep must skip, not delete.
func TestStagingSweep_LookupFailureKeepsTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := stagingDir(t, root, uuid.NewString())

	uc := NewFleetStagingSweeper(errReplicaRepo{newRTReplicaRepo()}, newFakeWorkloadRepo(), root)
	out, err := uc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep must not fail on a per-directory lookup error, got %v", err)
	}
	if out.Reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0 while the replica lookup is failing", out.Reclaimed)
	}
	if !dirExists(t, dir) {
		t.Fatalf("%s must survive: orphanhood could not be proven", dir)
	}
}

// TestStagingSweep_RefusesWithoutBothRepositories: the work root is shared by
// the replica path and the workload path, so half the evidence is no evidence.
func TestStagingSweep_RefusesWithoutBothRepositories(t *testing.T) {
	root := t.TempDir()
	dir := stagingDir(t, root, uuid.NewString())

	for name, uc := range map[string]*FleetStagingSweeper{
		"no replica repository":  NewFleetStagingSweeper(nil, newFakeWorkloadRepo(), root),
		"no workload repository": NewFleetStagingSweeper(newRTReplicaRepo(), nil, root),
	} {
		if _, err := uc.Sweep(context.Background()); err == nil {
			t.Fatalf("%s: Sweep should refuse to run", name)
		}
		if !dirExists(t, dir) {
			t.Fatalf("%s: nothing may be removed", name)
		}
	}
}

// TestStagingSweep_UnconfiguredOrMissingRootIsNoop: an unconfigured work root
// must never resolve to the process working directory, and a root that does not
// exist yet is not an error.
func TestStagingSweep_UnconfiguredOrMissingRootIsNoop(t *testing.T) {
	for name, root := range map[string]string{
		"unconfigured": "",
		"missing":      filepath.Join(t.TempDir(), "not-created-yet"),
	} {
		uc := NewFleetStagingSweeper(newRTReplicaRepo(), newFakeWorkloadRepo(), root)
		out, err := uc.Sweep(context.Background())
		if err != nil {
			t.Fatalf("%s root: %v", name, err)
		}
		if out.Reclaimed != 0 {
			t.Fatalf("%s root: reclaimed = %d, want 0", name, out.Reclaimed)
		}
	}
}
