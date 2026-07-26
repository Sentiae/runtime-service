package usecase

import (
	"bufio"
	"bytes"
	"compress/gzip"
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
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// ─────────────────────────────────────────────────────────────────────
// FleetVolumeRestorer use case — the P19 in-place restore path (D-184,
// SentiaeDB Phase 0). Snapshots existed before this; without a restore they
// were dead weight and host death meant customer data loss.
//
// In-place ONLY: the recovery point is swapped over the SAME resource's SAME
// volume. Restore-as-fork (restore into a NEW resource) is a later slice and is
// deliberately not designed in here.
// ─────────────────────────────────────────────────────────────────────

// AppHealthProbe is the subset of FleetProvisioner the restorer needs to decide
// whether the restored engine actually came back. *FleetProvision satisfies it.
type AppHealthProbe interface {
	Health(ctx context.Context, handle string) (FleetHealthOutput, error)
}

// AppHostAffinity reports the host a volume-bearing app's data is pinned to.
// This is the SAME seam the reconciler consults to decide whether a stateful app
// belongs to this host (fleet_orchestrator.ReconcileApp) — the restore sweep
// reuses it rather than inventing a second notion of "mine".
// *FleetVolumeManager satisfies it.
type AppHostAffinity interface {
	AffinityHost(ctx context.Context, appID uuid.UUID) (*uuid.UUID, bool, error)
}

var (
	_ AppHealthProbe  = (*FleetProvision)(nil)
	_ AppHostAffinity = (*FleetVolumeManager)(nil)
)

const (
	// prerestoreSuffix names the pre-restore original kept beside the live
	// backing file until the restored volume has booted healthy. It is the ONLY
	// anchor a rollback has, so it is written exactly once per restore chain and
	// never clobbered by a subsequent attempt.
	prerestoreSuffix = ".prerestore"
	// restoreInterruptedMsg is stamped on any resource of THIS host still in phase
	// restoring at boot: the process that owned the restore is gone. Re-issuing
	// RestoreResource is the recovery — it is idempotent by construction because
	// the swap leaves deterministic file states.
	restoreInterruptedMsg = "restore interrupted by restart; re-issue RestoreResource to recover"
)

// restoreStagingPath is THE name a restore stages a recovery point under, keyed
// by the recovery point (never by the volume). It is a function rather than an
// inline join because two places must agree on it exactly — the restore that
// creates the file (run) and the boot sweep that reclaims an abandoned one
// (reclaimStagingFiles). A drift between them would silently reintroduce the
// full-volume-size leak the sweep exists to close.
func restoreStagingPath(dir string, recoveryPointID uuid.UUID) string {
	return filepath.Join(dir, ".restore-"+recoveryPointID.String()+".tmp")
}

// restoreFromPhases are the phases a restore may be admitted from.
//
// `restoring` is deliberately EXCLUDED: the durable CAS is the cross-process
// admission gate, so a resource another instance is restoring can never be
// entered twice. A restore interrupted by a restart is released by the boot-time
// sweep (restoring → failed), which is how a re-issued RestoreResource becomes
// admissible again. Fail-closed safety does not ride on the phase — it rides on
// the volume's `restoring` status, which refuses every boot regardless.
var restoreFromPhases = []domain.FleetResourcePhase{
	domain.FleetResourcePhaseReady,
	domain.FleetResourcePhaseDegraded,
	domain.FleetResourcePhaseProvisioning,
	domain.FleetResourcePhaseFailed,
}

// FleetVolumeRestorer restores a dedicated resource's data volume in place from
// one of its recovery points. It composes the existing fleet machinery (the
// orchestrator's ScaleApp, the artifact store, the app health probe) and owns
// exactly one new thing: the ordering that makes each step crash-tolerant given
// the ones before it.
type FleetVolumeRestorer struct {
	resources repository.FleetResourceRepository
	volumes   repository.VolumeRepository
	replicas  repository.ReplicaRepository
	scaler    AppScaler
	health    AppHealthProbe
	store     ArtifactStore

	// active holds the resource ids with a LIVE restore goroutine in this
	// process, so a second RPC cannot enter a restore that is already running
	// (the durable phase CAS cannot distinguish that from a post-crash re-issue).
	active sync.Map // uuid.UUID → struct{}

	// selfHost + affinity scope the boot-time sweep to the resources whose data
	// lives on THIS host. Unset (zero uuid / nil) means ownership cannot be
	// determined, and the sweep then touches nothing.
	selfHost uuid.UUID
	affinity AppHostAffinity

	// baseCtx is the service root context: a restore must outlive the RPC that
	// asked for it (the RPC returns as soon as the phase is claimed).
	baseCtx context.Context
	wg      sync.WaitGroup

	// pgReady decides whether the restored engine ADMITS clients, over and above
	// the app health probe's process-alive + TCP-dial (see waitHealthy). A field
	// rather than a direct call so tests drive both verdicts without a live
	// engine; production is always probePostgresReady. Restores are postgres +
	// dedicated only (see Restore's tier gate), so the engine-specific probe is
	// exact here in a way it would not be on the general replica health path.
	pgReady func(ctx context.Context, host string, port int) error

	// Budgets. Fields (not constants) so tests can drive the state machine
	// without waiting on real timeouts.
	budget        time.Duration
	drainTimeout  time.Duration
	drainPoll     time.Duration
	healthTimeout time.Duration
	healthPoll    time.Duration
}

// NewFleetVolumeRestorer constructs the use case. baseCtx is the service root
// context used for the detached restore goroutine.
func NewFleetVolumeRestorer(
	baseCtx context.Context,
	resources repository.FleetResourceRepository,
	volumes repository.VolumeRepository,
	replicas repository.ReplicaRepository,
	scaler AppScaler,
	health AppHealthProbe,
	store ArtifactStore,
) *FleetVolumeRestorer {
	return &FleetVolumeRestorer{
		resources:     resources,
		volumes:       volumes,
		replicas:      replicas,
		scaler:        scaler,
		health:        health,
		store:         store,
		pgReady:       probePostgresReady,
		baseCtx:       baseCtx,
		budget:        30 * time.Minute,
		drainTimeout:  60 * time.Second,
		drainPoll:     500 * time.Millisecond,
		healthTimeout: 2 * time.Minute,
		healthPoll:    2 * time.Second,
	}
}

// SetHostScope wires this instance's fleet host identity and the app→host
// affinity seam, which together scope the boot-time sweep to the resources whose
// backing files live on THIS host. Wired after self-registration (the host id
// does not exist before it). Without it the sweep is a no-op — a restore live on
// another host must never be stamped by this one.
func (uc *FleetVolumeRestorer) SetHostScope(selfHost uuid.UUID, affinity AppHostAffinity) {
	uc.selfHost = selfHost
	uc.affinity = affinity
}

// Wait blocks until every in-flight restore goroutine has finished. Called on
// container Close so a restore's terminal phase is persisted before the DB pool
// is torn down.
func (uc *FleetVolumeRestorer) Wait() { uc.wg.Wait() }

// RestoreResourceInput is the wire-agnostic restore request: the resolved
// resource and the recovery point to restore it from. The handler resolves both
// (the recovery point strictly within the resource) so the org gate and the
// not-found translation happen at the boundary.
type RestoreResourceInput struct {
	Resource      *domain.FleetResource
	RecoveryPoint *domain.FleetResourceRecoveryPoint
}

// RestoreResourceOutput is the admission result: the resource handle and the
// phase it was moved into.
type RestoreResourceOutput struct {
	Handle string
	Phase  string
}

// Restore admits an in-place restore and returns immediately. Everything after
// admission (download, verify, drain, swap, verify-boot, rollback) runs in a
// detached goroutine on the service base context; callers poll GetResourceStatus
// for the outcome — phase `ready` with an empty last_error means the restore
// took, phase `ready` WITH a last_error means it was rolled back.
func (uc *FleetVolumeRestorer) Restore(ctx context.Context, in RestoreResourceInput) (RestoreResourceOutput, error) {
	out, err := uc.restore(ctx, in)
	// §22 counter. It counts ADMISSION only — the restore itself finishes in a
	// detached goroutine, and its outcome is the resource's phase + last_error, not
	// this return value. Counting it here as `ok` would otherwise be read as "the
	// restore worked".
	recordExecution("restore_resource_admit", outcomeFor(err))
	return out, err
}

func (uc *FleetVolumeRestorer) restore(ctx context.Context, in RestoreResourceInput) (RestoreResourceOutput, error) {
	var zero RestoreResourceOutput
	res, rp := in.Resource, in.RecoveryPoint
	if res == nil {
		return zero, domain.ErrResourceNotFound
	}
	// The recovery point must belong to THIS resource. The handler already scopes
	// its lookup; re-checking here means the use case cannot be handed a
	// mismatched pair by any future caller.
	if rp == nil || rp.ObjectKey == "" || rp.ResourceID != res.ID {
		return zero, domain.ErrRecoveryPointNotFound
	}
	if uc.store == nil {
		return zero, domain.ErrRestoreStoreUnavailable
	}
	if res.Tier != resourceTierDedicated {
		return zero, domain.ErrResourceTierUnsupported
	}
	// A tombstoned resource has no app to restore INTO — restoring its surviving
	// recovery points means creating a new resource, which is restore-as-fork.
	if res.Phase == domain.FleetResourcePhaseDecommissioned || res.AppID == nil {
		return zero, domain.ErrRestoreNoBackingApp
	}
	vol, err := uc.soleVolume(ctx, *res.AppID)
	if err != nil {
		return zero, err
	}

	if _, busy := uc.active.LoadOrStore(res.ID, struct{}{}); busy {
		return zero, domain.ErrRestoreInProgress
	}
	swapped, err := uc.resources.CompareAndSwapPhase(ctx, res.ID, restoreFromPhases, domain.FleetResourcePhaseRestoring)
	if err != nil {
		uc.active.Delete(res.ID)
		return zero, fmt.Errorf("claim restore phase: %w", err)
	}
	if !swapped {
		uc.active.Delete(res.ID)
		return zero, domain.ErrRestoreInProgress
	}
	if lerr := uc.resources.SetResourceLastError(ctx, res.ID, ""); lerr != nil {
		logger.FromContext(ctx).Warn("fleet restore: clear last_error", "resource_id", res.ID, "err", lerr)
	}

	// A resource already IN `restoring` was left there by a crashed attempt: its
	// on-disk state is unknown, so a failure that puts the phase back must not
	// claim it is ready/failed — degraded is the honest resting place.
	prevPhase := res.Phase
	if prevPhase == domain.FleetResourcePhaseRestoring {
		prevPhase = domain.FleetResourcePhaseDegraded
	}
	uc.start(res.ID, *res.AppID, rp, vol, prevPhase)
	return RestoreResourceOutput{Handle: res.ID.String(), Phase: string(domain.FleetResourcePhaseRestoring)}, nil
}

// SweepInterruptedRestores releases the restores this process owned when it
// died: for every resource of THIS host still in phase restoring it moves the
// phase restoring → failed and records why. `failed` is the honest phase — a
// restore that did not finish IS failed — and it is what makes a re-issued
// RestoreResource admissible again (see restoreFromPhases).
//
// It deliberately does NOT auto-resume. Safety is unaffected either way: the
// VOLUME stays `restoring`, so every boot is still refused until a restore
// completes and lowers the stand-off.
//
// Scoping is strict. A resource whose owning host cannot be determined (no host
// scope wired, no backing app, no pinned affinity, or a lookup error) is LEFT
// ALONE: skipping an ambiguous row is always safer than stamping a restore that
// is live on another host. Returns the number of resources released.
//
// Each released resource also has its abandoned restore STAGING files reclaimed
// (see reclaimStagingFiles) — a restore killed mid-copy leaves a file the size of
// the whole volume and nothing else ever removes it.
//
// Known residue, deliberately not covered: an interrupted restore whose resource
// has since left the `restoring` phase by some other route keeps its staging
// file, because this sweep only walks ListResourcesByPhase(restoring).
func (uc *FleetVolumeRestorer) SweepInterruptedRestores(ctx context.Context) (int, error) {
	log := logger.FromContext(ctx)
	if uc.selfHost == uuid.Nil || uc.affinity == nil {
		log.Warn("fleet restore sweep: skipped, this instance has no fleet host identity to scope by")
		return 0, nil
	}
	stuck, err := uc.resources.ListResourcesByPhase(ctx, domain.FleetResourcePhaseRestoring)
	if err != nil {
		return 0, fmt.Errorf("list restoring resources: %w", err)
	}
	released := 0
	for i := range stuck {
		res := &stuck[i]
		if !uc.ownedByThisHost(ctx, res) {
			continue
		}
		// CAS, not a blind update: the row may have left `restoring` between the
		// list and here (another instance finishing a restore of the same resource).
		swapped, cerr := uc.resources.CompareAndSwapPhase(ctx, res.ID,
			[]domain.FleetResourcePhase{domain.FleetResourcePhaseRestoring}, domain.FleetResourcePhaseFailed)
		if cerr != nil {
			log.Warn("fleet restore sweep: release phase", "resource_id", res.ID, "err", cerr)
			continue
		}
		if !swapped {
			continue
		}
		if serr := uc.resources.SetResourceLastError(ctx, res.ID, restoreInterruptedMsg); serr != nil {
			log.Warn("fleet restore sweep: stamp last_error", "resource_id", res.ID, "err", serr)
		}
		log.Warn("fleet restore sweep: restore interrupted by restart; resource released to failed, boots stay refused until it is re-issued",
			"resource_id", res.ID)
		released++
		uc.reclaimStagingFiles(ctx, res)
	}
	return released, nil
}

// reclaimStagingFiles removes the staging files left behind by this resource's
// interrupted restores. A restore that dies mid-copy (panic in run, process
// kill) never reaches the error branches that remove `staged`, and the file it
// leaves is the size of the whole volume.
//
// Why it is safe to remove them HERE and nowhere else. The staging name is keyed
// by RECOVERY POINT, and the directory is shared by every volume on the host, so
// a sweep over the DIRECTORY (a `.restore-*.tmp` glob) could destroy another
// volume's in-flight staging file mid-copy. This walks the other way: it probes
// the exact name of each recovery point OF THIS RESOURCE, and a recovery point
// belongs to exactly one resource — so a match can only ever be a file that a
// restore of this resource created. Ownership is proven, never guessed.
//
// And it is safe to remove it NOW because the caller has just CAS'd this
// resource out of `restoring`: no restore of it can be in flight, so nothing can
// be writing any of these paths.
//
// Probing every recovery point rather than only the last one is intentional — it
// also reclaims the residue of earlier interrupted attempts. This runs at boot
// over released resources only, so the extra stats cost nothing.
//
// Best-effort throughout: nothing here may fail the sweep. A missing file is the
// normal case (most restores exit cleanly), so it is not an error.
func (uc *FleetVolumeRestorer) reclaimStagingFiles(ctx context.Context, res *domain.FleetResource) {
	log := logger.FromContext(ctx)
	if res.AppID == nil {
		return
	}
	vol, err := uc.soleVolume(ctx, *res.AppID)
	if err != nil {
		log.Warn("fleet restore sweep: resolve volume to reclaim restore staging files",
			"resource_id", res.ID, "err", err)
		return
	}
	dir := filepath.Dir(vol.BackingPath)
	points, err := uc.resources.ListRecoveryPoints(ctx, res.ID)
	if err != nil {
		log.Warn("fleet restore sweep: list recovery points to reclaim restore staging files",
			"resource_id", res.ID, "err", err)
		return
	}
	for i := range points {
		rp := &points[i]
		path := restoreStagingPath(dir, rp.ID)
		// Stat first: the reclaimed size is the whole point of the log line, and it
		// is unrecoverable after the remove.
		fi, serr := os.Stat(path)
		if serr != nil {
			if !os.IsNotExist(serr) {
				log.Warn("fleet restore sweep: stat abandoned restore staging file", "path", path, "err", serr)
			}
			continue
		}
		if rerr := os.Remove(path); rerr != nil {
			if !os.IsNotExist(rerr) {
				log.Warn("fleet restore sweep: remove abandoned restore staging file", "path", path, "err", rerr)
			}
			continue
		}
		log.Info("fleet restore sweep: reclaimed abandoned restore staging file",
			"resource_id", res.ID, "recovery_point_id", rp.ID, "path", path, "bytes_reclaimed", fi.Size())
	}
}

// ownedByThisHost reports whether a resource's data lives on this host, via the
// SAME app→volume affinity the reconciler uses for stateful apps. Anything it
// cannot answer positively is a "no".
func (uc *FleetVolumeRestorer) ownedByThisHost(ctx context.Context, res *domain.FleetResource) bool {
	if res.AppID == nil {
		return false
	}
	host, pinned, err := uc.affinity.AffinityHost(ctx, *res.AppID)
	if err != nil {
		logger.FromContext(ctx).Warn("fleet restore sweep: affinity host lookup, leaving resource alone",
			"resource_id", res.ID, "err", err)
		return false
	}
	return pinned && host != nil && *host == uc.selfHost
}

// soleVolume resolves the app's single materialized data volume. In-place
// restore swaps ONE backing file: with zero or several it must refuse rather
// than guess which one the recovery point belongs to.
func (uc *FleetVolumeRestorer) soleVolume(ctx context.Context, appID uuid.UUID) (*domain.Volume, error) {
	vols, err := uc.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	if len(vols) != 1 || vols[0].BackingPath == "" {
		return nil, domain.ErrRestoreVolumeAmbiguous
	}
	v := vols[0]
	return &v, nil
}

// start launches the detached restore. Restore has already returned by the time
// this runs, so it uses the service base context, is tracked in the waitgroup,
// and recovers from a panic (a panicking restore must still release the
// in-process claim).
func (uc *FleetVolumeRestorer) start(resourceID, appID uuid.UUID, rp *domain.FleetResourceRecoveryPoint, vol *domain.Volume, prevPhase domain.FleetResourcePhase) {
	uc.wg.Add(1)
	go func() {
		defer uc.wg.Done()
		defer uc.active.Delete(resourceID)
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(uc.baseCtx).Error("fleet restore panicked",
					"resource_id", resourceID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(uc.baseCtx, uc.budget)
		defer cancel()
		uc.run(ctx, resourceID, appID, rp, vol, prevPhase)
	}()
}

// run executes the restore. Every step is ordered so that a crash at any point
// leaves a state the next re-issued restore can reason about.
func (uc *FleetVolumeRestorer) run(ctx context.Context, resourceID, appID uuid.UUID, rp *domain.FleetResourceRecoveryPoint, vol *domain.Volume, prevPhase domain.FleetResourcePhase) {
	log := logger.FromContext(ctx)
	live := vol.BackingPath
	dir := filepath.Dir(live)
	staged := restoreStagingPath(dir, rp.ID)
	pre := live + prerestoreSuffix

	// Step 4 — fetch and VERIFY before anything live is touched. Staging in the
	// backing file's own directory is what makes the later rename atomic (same
	// filesystem); it is also where the snapshotter stages its copies.
	if err := uc.stage(ctx, rp, staged); err != nil {
		_ = os.Remove(staged)
		uc.abandon(ctx, resourceID, prevPhase, fmt.Errorf("stage recovery point: %w", err))
		return
	}
	log.Info("fleet restore: recovery point staged and verified",
		"resource_id", resourceID, "recovery_point_id", rp.ID, "path", staged)

	// Step 5 — raise the boot stand-off. From here BootReplica refuses this
	// volume, so neither a reconciler tick nor an ingress wake can open the
	// backing file we are about to rename.
	if err := uc.setVolumeStatus(ctx, vol, domain.VolumeStatusRestoring, vol.AttachedReplica); err != nil {
		_ = os.Remove(staged)
		uc.abandon(ctx, resourceID, prevPhase, err)
		return
	}

	// Step 6 — drain the engine and confirm nothing holds the volume.
	if err := uc.drain(ctx, appID); err != nil {
		_ = os.Remove(staged)
		uc.revive(ctx, resourceID, appID, vol, prevPhase, fmt.Errorf("drain before swap: %w", err))
		return
	}

	// Step 7 — the swap. VM down, boots refused.
	if err := swapIn(staged, live, pre); err != nil {
		_ = os.Remove(staged)
		// The live path may now be missing (parked as .prerestore). Put the
		// original back before handing the resource back to service.
		if rerr := restorePrerestore(live, pre); rerr != nil {
			uc.setVolumeAvailable(ctx, vol)
			uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
				fmt.Errorf("swap failed (%w) and the pre-restore volume could not be put back: %v", err, rerr))
			return
		}
		uc.revive(ctx, resourceID, appID, vol, prevPhase, fmt.Errorf("swap restored volume: %w", err))
		return
	}
	log.Info("fleet restore: volume swapped", "resource_id", resourceID, "backing_path", live)

	// Step 8 — release the stand-off and boot on the restored file.
	uc.setVolumeAvailable(ctx, vol)
	if err := uc.scale(ctx, appID, 1); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded, fmt.Errorf("boot restored volume: %w", err))
		return
	}

	// Step 9 — the restore is only real once the engine serves from it.
	healthErr := uc.waitHealthy(ctx, appID)
	if healthErr == nil {
		// What is proven here is exactly: these bytes were restored IN PLACE, an
		// engine booted on them, and it admitted a client. That is what the flag is
		// now NAMED after (RestoredInPlaceOK), so nothing downstream can read it as
		// a drill result. `Verified` in the P19 port doc means a G1
		// restore-VERIFICATION DRILL — restore into a throwaway target and assert
		// the CONTENT — which does not exist yet; it lands right here when it does,
		// as a SECOND fact, not by widening this one.
		if verr := uc.resources.MarkRecoveryPointRestoredInPlace(ctx, rp.ID); verr != nil {
			log.Warn("fleet restore: mark recovery point restored-in-place", "recovery_point_id", rp.ID, "err", verr)
		}
		if rerr := os.Remove(pre); rerr != nil && !os.IsNotExist(rerr) {
			log.Warn("fleet restore: remove pre-restore volume", "path", pre, "err", rerr)
		}
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseReady, nil)
		log.Info("fleet restore: complete", "resource_id", resourceID, "recovery_point_id", rp.ID)
		return
	}

	// Step 10 — the restored volume did not come up: roll back to the original.
	log.Error("fleet restore: restored volume failed to boot, rolling back",
		"resource_id", resourceID, "recovery_point_id", rp.ID, "err", healthErr)
	uc.rollback(ctx, resourceID, appID, rp, vol, healthErr)
}

// rollback puts the pre-restore volume back and returns the resource to service.
// The failed restore is KEPT (renamed aside) for forensics — the operator needs
// to see what the recovery point actually contained.
func (uc *FleetVolumeRestorer) rollback(ctx context.Context, resourceID, appID uuid.UUID, rp *domain.FleetResourceRecoveryPoint, vol *domain.Volume, cause error) {
	live := vol.BackingPath
	pre := live + prerestoreSuffix
	failed := live + ".failed-" + rp.ID.String()

	// Raise the stand-off again: the rollback renames the same path.
	if err := uc.setVolumeStatus(ctx, vol, domain.VolumeStatusRestoring, nil); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("rollback could not re-raise the boot stand-off after %w: %v", cause, err))
		return
	}
	if err := uc.drain(ctx, appID); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("rollback could not drain after %w: %v", cause, err))
		return
	}
	if err := swapBack(live, pre, failed); err != nil {
		uc.setVolumeAvailable(ctx, vol)
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("rollback failed after %w: %v", cause, err))
		return
	}
	uc.setVolumeAvailable(ctx, vol)
	if err := uc.scale(ctx, appID, 1); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("rollback could not boot the pre-restore volume after %w: %v", cause, err))
		return
	}
	if err := uc.waitHealthy(ctx, appID); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("rollback: pre-restore volume also failed to boot after %w: %v", cause, err))
		return
	}
	// Back in service on the ORIGINAL data — ready, but last_error stays set so a
	// poller can tell a rolled-back restore from a successful one.
	uc.finish(ctx, resourceID, domain.FleetResourcePhaseReady,
		fmt.Errorf("restore from %s rolled back: %w", rp.ObjectKey, cause))
}

// stage streams the recovery point into the staging file, fsyncs it and its
// directory, and verifies it against the catalog.
//
// Two things happen on the way in. The object is DECOMPRESSED when it is a gzip
// stream (see uploadSnapshotFileHashed for why snapshots are compressed), and
// the staging file is written SPARSELY — a 20GB-nominal volume holding 4.6GB
// must not materialize 20GB of zeros on the way back, or the restore reproduces
// the disk cost the snapshot side just eliminated.
//
// The hash is taken over the bytes AS DOWNLOADED (== as stored), which is what
// the recovery point's checksum covers, while the SIZE check compares the
// decompressed length against the logical volume size. Both are verified before
// anything live is touched.
func (uc *FleetVolumeRestorer) stage(ctx context.Context, rp *domain.FleetResourceRecoveryPoint, dst string) error {
	rc, err := uc.store.Get(rp.ObjectKey)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", rp.ObjectKey, err)
	}
	defer rc.Close()

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file %s: %w", dst, err)
	}
	// Close exactly once on every exit — error, panic, or the explicit close
	// below whose error must be checked (it is the last chance to see a
	// write-back failure before the swap).
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	h := sha256.New()
	body, err := decompressedBody(io.TeeReader(rc, h))
	if err != nil {
		return fmt.Errorf("read %s: %w", rp.ObjectKey, err)
	}
	restored, copyErr := copySparse(f, body)
	if copyErr != nil {
		return fmt.Errorf("write staging file %s: %w", dst, copyErr)
	}
	// fsync before the rename: the whole point of the restore is durability, so
	// the bytes must be on the platter, not in page cache, when we swap.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync staging file %s: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging file %s: %w", dst, err)
	}
	closed = true
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return verifyRecoveryPoint(ctx, rp, restored, hex.EncodeToString(h.Sum(nil)))
}

// decompressedBody returns the volume image behind a stored object, unwrapping
// gzip when the object is compressed.
//
// The format is DETECTED from the two-byte gzip magic rather than recorded,
// because objects written before compression landed are raw images and they are
// still a customer's only recovery points — a restore that failed on them would
// fail with no visible reason. This is format detection on a durable artifact,
// not a compatibility shim: it is three lines and it never grows a second codec
// (Phase 2's CoW block plane replaces this path wholesale).
func decompressedBody(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return br, nil
	}
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	return zr, nil
}

// sparseChunk is the granularity at which the decompressed stream is scanned
// for holes. Reads are filled completely, so chunks stay aligned to multiples
// of this size and a skipped chunk always covers whole filesystem blocks.
const sparseChunk = 256 << 10

// zeroChunk is the comparison operand for "this chunk is a hole". Package-level
// so the copy loop allocates nothing per chunk.
var zeroChunk = make([]byte, sparseChunk)

// copySparse writes r into f, SKIPPING runs of zeros instead of writing them,
// and returns the logical length written.
//
// A hole in the source volume decompresses back into zeros, and writing them
// would allocate real blocks — the staged file would cost the volume's NOMINAL
// size on a host that only ever had room for its real data. Seeking past them
// (WriteAt at the advanced offset) keeps the staged file as sparse as the volume
// it came from. The residual cost is that the zeros are still decompressed into
// memory and scanned; only the writes are avoided.
func copySparse(f *os.File, r io.Reader) (int64, error) {
	buf := make([]byte, sparseChunk)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if chunk := buf[:n]; !bytes.Equal(chunk, zeroChunk[:n]) {
				if _, werr := f.WriteAt(chunk, off); werr != nil {
					return off, fmt.Errorf("write at offset %d: %w", off, werr)
				}
			}
			off += int64(n)
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			// A short final chunk and a TRUNCATED source are indistinguishable
			// here; that is safe because the caller verifies the decompressed
			// length and the downloaded checksum before anything live is touched.
			// The file's LENGTH comes from Truncate, not from the last write: a
			// volume ending in a hole has nothing written anywhere near its end.
			if terr := f.Truncate(off); terr != nil {
				return off, fmt.Errorf("set staging file length to %d: %w", off, terr)
			}
			return off, nil
		default:
			return off, err
		}
	}
}

// verifyRecoveryPoint checks the staged image against the catalog. `staged` is
// the DECOMPRESSED length (what the volume will be) and `sum` is the sha256 of
// the bytes as DOWNLOADED (what the checksum column has always covered) — for a
// raw, uncompressed object the two are the same bytes, which is why legacy
// recovery points verify unchanged. A recovery point written before D-184
// carries no checksum: it is size-verified only, and that limitation is LOGGED
// rather than passed off as integrity.
func verifyRecoveryPoint(ctx context.Context, rp *domain.FleetResourceRecoveryPoint, staged int64, sum string) error {
	if rp.SizeBytes <= 0 && rp.Checksum == "" {
		return fmt.Errorf("%w: recovery point %s records neither size nor checksum", domain.ErrRestoreIntegrity, rp.ID)
	}
	if rp.SizeBytes > 0 && staged != rp.SizeBytes {
		return fmt.Errorf("%w: staged %d bytes, catalog records %d", domain.ErrRestoreIntegrity, staged, rp.SizeBytes)
	}
	if rp.Checksum == "" {
		logger.FromContext(ctx).Warn("fleet restore: legacy recovery point carries NO checksum — only its size could be verified",
			"recovery_point_id", rp.ID, "object_key", rp.ObjectKey, "size_bytes", staged)
		return nil
	}
	if !strings.EqualFold(sum, rp.Checksum) {
		return fmt.Errorf("%w: staged sha256 %s, catalog records %s", domain.ErrRestoreIntegrity, sum, rp.Checksum)
	}
	return nil
}

// drain scales the app to zero and waits until no replica row remains, i.e.
// nothing can still hold the backing file open.
func (uc *FleetVolumeRestorer) drain(ctx context.Context, appID uuid.UUID) error {
	if err := uc.scale(ctx, appID, 0); err != nil {
		return err
	}
	deadline := time.Now().Add(uc.drainTimeout)
	for {
		reps, err := uc.replicas.ListByApp(ctx, appID)
		if err != nil {
			return fmt.Errorf("list replicas: %w", err)
		}
		if len(reps) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("volume still occupied by %d replica(s) after %s", len(reps), uc.drainTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(uc.drainPoll):
		}
	}
}

// waitHealthy polls the app until it is serving, or the budget elapses.
//
// "Serving" here is a STRICTER claim than fleet health. The app health probe is
// process-alive + a TCP dial of the guest port, which proves only that something
// is listening: a restored Postgres whose pg_hba.conf came back torn passes it
// while refusing every client (#p19-restore-false-green-health, observed live
// twice). So a restore — the one operation that hands customers back data they
// asked to be trusted — additionally requires the engine to ADMIT a connection.
func (uc *FleetVolumeRestorer) waitHealthy(ctx context.Context, appID uuid.UUID) error {
	deadline := time.Now().Add(uc.healthTimeout)
	var last error
	for {
		last = uc.serving(ctx, appID)
		if last == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(uc.healthPoll):
		}
	}
}

// serving is one round of the two gates: fleet health first (it is cheap and it
// is what reports a VM that never came back at all), then the engine-admits
// probe.
func (uc *FleetVolumeRestorer) serving(ctx context.Context, appID uuid.UUID) error {
	h, err := uc.health.Health(ctx, appID.String())
	if err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	if !h.Healthy {
		return fmt.Errorf("app not healthy (state=%s message=%s)", h.State, h.Message)
	}
	return uc.engineAdmits(ctx, appID)
}

// engineAdmits checks that every resident replica of the app lets a client
// through pg_hba to authentication, using the credential-free probe.
//
// Fail-closed throughout: a replica listed as resident with no guest address, an
// app with no resident replica at all, and a repository error are all failures,
// because each of them means the restore cannot be PROVEN to have produced a
// usable database — and an unprovable restore must never be reported as one.
func (uc *FleetVolumeRestorer) engineAdmits(ctx context.Context, appID uuid.UUID) error {
	if uc.pgReady == nil {
		return fmt.Errorf("%w: no readiness probe is wired, so the restored engine cannot be confirmed usable", domain.ErrRestoreEngineNotAdmitting)
	}
	reps, err := uc.replicas.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("%w: list replicas: %w", domain.ErrRestoreEngineNotAdmitting, err)
	}
	probed := 0
	for i := range reps {
		r := &reps[i]
		if r.State != domain.ReplicaStateResident {
			continue
		}
		if r.GuestIP == "" || r.Port <= 0 {
			return fmt.Errorf("%w: replica %s is resident but carries no guest address to probe", domain.ErrRestoreEngineNotAdmitting, r.ID)
		}
		if perr := uc.pgReady(ctx, r.GuestIP, r.Port); perr != nil {
			return fmt.Errorf("%w: replica %s: %w", domain.ErrRestoreEngineNotAdmitting, r.ID, perr)
		}
		probed++
	}
	if probed == 0 {
		return fmt.Errorf("%w: app %s has no resident replica to probe", domain.ErrRestoreEngineNotAdmitting, appID)
	}
	return nil
}

// scale sets the app's desired replica count through the orchestrator.
func (uc *FleetVolumeRestorer) scale(ctx context.Context, appID uuid.UUID, replicas int) error {
	known, err := uc.scaler.ScaleApp(ctx, appID, replicas)
	if err != nil {
		return fmt.Errorf("scale app to %d: %w", replicas, err)
	}
	if !known {
		return fmt.Errorf("scale app to %d: %w", replicas, domain.ErrFleetAppNotFound)
	}
	return nil
}

// setVolumeStatus persists a volume status transition.
func (uc *FleetVolumeRestorer) setVolumeStatus(ctx context.Context, vol *domain.Volume, st domain.VolumeStatus, attached *uuid.UUID) error {
	vol.Status = st
	vol.AttachedReplica = attached
	vol.UpdatedAt = time.Now().UTC()
	if err := uc.volumes.Update(ctx, vol); err != nil {
		return fmt.Errorf("set volume status %s: %w", st, err)
	}
	return nil
}

// setVolumeAvailable lowers the stand-off. A failure here is logged, not
// propagated: the caller is already on a path that ends in a terminal phase,
// and a stuck `restoring` volume is the FAIL-CLOSED side (boots refused).
func (uc *FleetVolumeRestorer) setVolumeAvailable(ctx context.Context, vol *domain.Volume) {
	if err := uc.setVolumeStatus(ctx, vol, domain.VolumeStatusAvailable, nil); err != nil {
		logger.FromContext(ctx).Error("fleet restore: lower boot stand-off", "volume_id", vol.ID, "err", err)
	}
}

// abandon aborts a restore that never touched the live volume: the resource goes
// back to the phase it came from, with the reason recorded.
func (uc *FleetVolumeRestorer) abandon(ctx context.Context, resourceID uuid.UUID, prevPhase domain.FleetResourcePhase, cause error) {
	logger.FromContext(ctx).Error("fleet restore: aborted before touching the live volume",
		"resource_id", resourceID, "err", cause)
	uc.finish(ctx, resourceID, prevPhase, cause)
}

// revive puts a drained-but-not-yet-swapped resource back into service on its
// ORIGINAL volume, then abandons the restore.
func (uc *FleetVolumeRestorer) revive(ctx context.Context, resourceID, appID uuid.UUID, vol *domain.Volume, prevPhase domain.FleetResourcePhase, cause error) {
	uc.setVolumeAvailable(ctx, vol)
	if err := uc.scale(ctx, appID, 1); err != nil {
		uc.finish(ctx, resourceID, domain.FleetResourcePhaseDegraded,
			fmt.Errorf("restore aborted (%w) and the original volume could not be brought back: %v", cause, err))
		return
	}
	uc.abandon(ctx, resourceID, prevPhase, cause)
}

// finish records the terminal phase and last_error of a restore. A nil cause
// clears last_error (only a clean success does that).
func (uc *FleetVolumeRestorer) finish(ctx context.Context, resourceID uuid.UUID, phase domain.FleetResourcePhase, cause error) {
	log := logger.FromContext(ctx)
	if err := uc.resources.UpdateResourcePhase(ctx, resourceID, phase); err != nil {
		log.Error("fleet restore: persist terminal phase", "resource_id", resourceID, "phase", phase, "err", err)
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if err := uc.resources.SetResourceLastError(ctx, resourceID, msg); err != nil {
		log.Error("fleet restore: persist last_error", "resource_id", resourceID, "err", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The swap file-state machine. Plain renames are sufficient (no renameat2 /
// RENAME_EXCHANGE): the stand-off has already closed every reader, and each
// intermediate state below is one a re-issued restore can recognize.
// ─────────────────────────────────────────────────────────────────────

// swapIn installs the staged volume at the live path, preserving the pre-restore
// original exactly once.
//
// When `pre` already exists an EARLIER restore was interrupted: that file is the
// only surviving original, so it is never clobbered — the current `live` is then
// a half-restored artifact and is dropped instead.
//
// When NEITHER file exists there is nothing to park, and the swap proceeds. This
// is the case where the backing file was LOST — a dead disk, a stray delete, an
// unfinished teardown — and it is precisely the case a restore exists to fix: a
// good recovery point is on hand and the resource is otherwise unrecoverable.
// Proceeding is provably LOSSLESS, not a judgement call: both paths have just
// been established ABSENT, so the install can overwrite no surviving copy of the
// customer's data. Refusing, as this did, cost the single most valuable recovery
// path to save nothing. The `pre`-exists branch above already tolerates a missing
// live file on exactly this reasoning. Rollback is unaffected and still fails
// honestly: with no anchor there is genuinely nowhere to go back to
// (domain.ErrRestoreNoPrerestoreAnchor).
func swapIn(staged, live, pre string) error {
	_, err := os.Stat(pre)
	switch {
	case err == nil:
		if rerr := os.Remove(live); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("drop half-restored volume %s: %w", live, rerr)
		}
	case os.IsNotExist(err):
		// Stat before rename so "the live file is gone" is a distinguishable state
		// rather than a rename error that has to be reverse-engineered.
		_, lerr := os.Stat(live)
		switch {
		case lerr == nil:
			if rerr := os.Rename(live, pre); rerr != nil {
				return fmt.Errorf("park pre-restore volume %s: %w", live, rerr)
			}
		case !os.IsNotExist(lerr):
			return fmt.Errorf("stat %s: %w", live, lerr)
		}
	default:
		return fmt.Errorf("stat %s: %w", pre, err)
	}
	if rerr := os.Rename(staged, live); rerr != nil {
		return fmt.Errorf("install restored volume at %s: %w", live, rerr)
	}
	return syncDir(filepath.Dir(live))
}

// swapBack rolls the live path back to the pre-restore original, keeping the
// failed restore aside for forensics.
//
// A missing anchor is TERMINAL here, unlike in swapIn: the rollback's whole job
// is to reinstate data that only the anchor holds, so without it there is
// nothing to reinstate and no retry can produce one. The caller records the
// resource degraded on this — the honest resting place.
func swapBack(live, pre, failed string) error {
	if _, err := os.Stat(pre); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", domain.ErrRestoreNoPrerestoreAnchor, pre)
		}
		return fmt.Errorf("pre-restore volume %s unavailable: %w", pre, err)
	}
	if err := os.Rename(live, failed); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("park failed restore %s: %w", live, err)
	}
	if err := os.Rename(pre, live); err != nil {
		return fmt.Errorf("reinstate pre-restore volume at %s: %w", live, err)
	}
	return syncDir(filepath.Dir(live))
}

// restorePrerestore puts the parked original back when the swap failed halfway
// (live already renamed to pre, staged not yet installed). A live file that is
// already in place is left alone.
//
// It reports the same terminal anchor error as swapBack: reaching here with
// neither a live file nor an anchor means the swap failed on a volume whose
// backing file was already lost, so there is nothing to put back and the caller
// must degrade rather than pretend a recovery happened.
func restorePrerestore(live, pre string) error {
	if _, err := os.Stat(live); err == nil {
		return nil
	}
	if _, err := os.Stat(pre); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", domain.ErrRestoreNoPrerestoreAnchor, pre)
		}
		return fmt.Errorf("pre-restore volume %s unavailable: %w", pre, err)
	}
	if err := os.Rename(pre, live); err != nil {
		return fmt.Errorf("reinstate pre-restore volume at %s: %w", live, err)
	}
	return syncDir(filepath.Dir(live))
}

// syncDir fsyncs a directory so a rename into it is durable across power loss —
// without this the rename can be lost while the file's data survives, leaving
// the volume pointing at the wrong inode after a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}
