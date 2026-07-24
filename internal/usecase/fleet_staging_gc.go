package usecase

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// stagingOrphanGrace is how long a staging directory must have been UNTOUCHED
// before the sweep will even consider it.
//
// It is not tidiness, it is a second lock on the safety argument. The sweep's
// primary proof is the database (below), which holds because the row is always
// written before the directory is created. The grace period means that even if
// some future path ever inverted that order, the window it would have to lose in
// is ten minutes wide rather than microseconds — and nothing is lost by waiting,
// since an orphan stays an orphan forever.
const stagingOrphanGrace = 10 * time.Minute

// StagingSweepOutput reports what one sweep reclaimed. Bytes is the APPARENT
// size of the removed trees (sparse rootfs images occupy less on disk), which is
// the number that matters for the "no space left on device" the leak produced.
type StagingSweepOutput struct {
	Reclaimed int
	Bytes     int64
}

// FleetStagingSweeper reclaims orphaned per-workload materialize staging
// directories under the image-boot work root (#fleet-image-staging-dirs-no-gc).
//
// A boot that fails now cleans up after itself (FleetReplicaRuntime.markDead),
// but that only helps FUTURE boots: a host today already carries the directories
// left by every boot that failed before the fix, plus any left by a process that
// was killed mid-boot. Those are only reclaimable by looking at the disk.
//
// It is HOST-LOCAL by construction and needs no host scoping (unlike the
// interrupted-restore sweep, which mutates shared control-plane rows): it only
// ever removes files on the filesystem it is running on, and a directory here
// was created here.
type FleetStagingSweeper struct {
	// Both repositories are required, because the work root is SHARED: the
	// resident path names its directories by REPLICA id (fleet_replica_runtime)
	// and the test/job/fallback path names its own by IMAGE WORKLOAD id
	// (fleet_provision). A sweep that consulted only one of them would delete a
	// live job's rootfs out from under a running VM.
	replicas  repository.ReplicaRepository
	workloads repository.ImageWorkloadRepository
	workDir   string
}

// NewFleetStagingSweeper constructs the use case. workDir is the image-boot work
// root (cfg.ImageBoot.WorkDir) — the same root both boot paths stage under.
func NewFleetStagingSweeper(
	replicas repository.ReplicaRepository,
	workloads repository.ImageWorkloadRepository,
	workDir string,
) *FleetStagingSweeper {
	return &FleetStagingSweeper{replicas: replicas, workloads: workloads, workDir: workDir}
}

// Sweep removes every staging directory under the work root that can be proven
// to be an orphan, and reports what it reclaimed.
//
// ⚠ THE SAFETY ARGUMENT — deleting a live VM's rootfs would be far worse than
// the leak, so every step is a refusal by default. A directory is removed ONLY
// when ALL of the following hold; anything unproven is skipped, never guessed:
//
//  1. it is a direct child of the work root and a real directory (a file, a
//     symlink, or anything nested deeper is left alone);
//  2. its name parses as a uuid — a name that does not is not something this
//     service created, so it is never interpreted;
//  3. it has not been modified for stagingOrphanGrace;
//  4. the replica repository positively answers "no such replica" — any other
//     error (including a DB outage) counts as "might be live" and skips;
//  5. the image-workload repository positively answers "no such workload", with
//     the same treatment of errors.
//
// Both lookups are required to conclude "orphan" because both boot paths stage
// under this same root. A replica row is created before BootReplica materializes,
// and a workload row before Provision materializes, so a directory whose id
// resolves to neither row can only be one whose owner has already been deleted.
//
// Errors are LOGGED PER DIRECTORY and the sweep continues: one unreadable entry
// must not strand the rest of a full disk. Only a failure to read the work root
// itself is returned.
func (uc *FleetStagingSweeper) Sweep(ctx context.Context) (StagingSweepOutput, error) {
	var out StagingSweepOutput
	if uc.workDir == "" {
		return out, nil
	}
	// Without both repositories orphanhood is not decidable, and an undecidable
	// sweep must do nothing rather than delete on half the evidence.
	if uc.replicas == nil || uc.workloads == nil {
		return out, errors.New("staging sweep needs both the replica and workload repositories to identify an orphan")
	}

	entries, err := os.ReadDir(uc.workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("read staging root %s: %w", uc.workDir, err)
	}

	log := logger.FromContext(ctx)
	cutoff := time.Now().Add(-stagingOrphanGrace)
	for _, e := range entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if !e.IsDir() {
			continue
		}
		id, perr := uuid.Parse(e.Name())
		if perr != nil {
			// Not a name this service minted. Never guess at what it belongs to.
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		orphan, oerr := uc.isOrphan(ctx, id)
		if oerr != nil {
			log.Warn("fleet staging gc: cannot decide whether a staging dir is orphaned, leaving it alone",
				"dir", e.Name(), "err", oerr)
			continue
		}
		if !orphan {
			continue
		}

		path := filepath.Join(uc.workDir, e.Name())
		size := treeSize(path)
		if rerr := os.RemoveAll(path); rerr != nil {
			log.Warn("fleet staging gc: remove orphaned staging dir", "path", path, "err", rerr)
			continue
		}
		out.Reclaimed++
		out.Bytes += size
		log.Info("fleet staging gc: reclaimed orphaned staging dir",
			"path", path, "bytes", size, "modified_at", info.ModTime().UTC())
	}
	return out, nil
}

// isOrphan reports whether id names neither a live replica nor a live image
// workload. Any lookup error other than the not-found sentinel is returned, so
// the caller keeps the directory (fail-safe).
func (uc *FleetStagingSweeper) isOrphan(ctx context.Context, id uuid.UUID) (bool, error) {
	if _, err := uc.replicas.FindByID(ctx, id); err == nil {
		return false, nil
	} else if !errors.Is(err, domain.ErrReplicaNotFound) {
		return false, fmt.Errorf("look up replica %s: %w", id, err)
	}
	if _, err := uc.workloads.FindByID(ctx, id); err == nil {
		return false, nil
	} else if !errors.Is(err, domain.ErrWorkloadNotFound) {
		return false, fmt.Errorf("look up workload %s: %w", id, err)
	}
	return true, nil
}

// treeSize sums the apparent size of a directory tree. Best-effort: it feeds a
// log line, so an unreadable entry contributes zero rather than aborting a
// reclaim that is already justified.
func treeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		// An unreadable entry is skipped rather than propagated: a size for a log
		// line must never fail a reclaim that is already justified.
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
