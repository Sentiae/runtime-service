package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/port/gateway"
	"github.com/sentiae/runtime-service/internal/repository"
)

// residentPGPort is the fixed in-guest port a dedicated Postgres data-VM listens
// on (mirrors the dedicated provision descriptor). Referenced by the resource
// provisioner's endpoint; kept here so the snapshot + provision paths agree.
const residentPGPort = 5432

// ─────────────────────────────────────────────────────────────────────
// VMPauser port — pauses/resumes a running microVM around a crash-consistent
// backing-file copy. The firecracker Provider satisfies it (Pause/Resume).
// ─────────────────────────────────────────────────────────────────────

// VMPauser pauses and resumes a running microVM by its API socket. Pausing
// suspends the guest's vCPUs; Resume releases them. The firecracker Provider
// implements this. It is NOT sufficient for a consistent data-disk copy on its
// own — see snapshotVolume, which pairs it with the guest freeze.
type VMPauser interface {
	Pause(ctx context.Context, socketPath string) error
	Resume(ctx context.Context, socketPath string) error
}

// ReplicaRecycler kills a replica's microVM. It is the snapshotter's escalation
// path for a guest that will not thaw: the replica row goes away with the VM and
// the reconciler's shortfall step boots a replacement on the SAME host and the
// SAME backing file within a tick (fleet_orchestrator.ReconcileApp), which is
// how "kill" becomes "reboot" without this use case owning any boot mechanics.
// *FleetReplicaRuntime satisfies it.
type ReplicaRecycler interface {
	DecommissionReplica(ctx context.Context, replicaID uuid.UUID) error
}

var _ ReplicaRecycler = (*FleetReplicaRuntime)(nil)

const (
	// defaultThawDeadline bounds the post-copy thaw retries. The copy is already
	// consistent by then, so this is only about not leaving guest writers blocked.
	defaultThawDeadline = 10 * time.Second
	// defaultThawRetry is the pause between thaw attempts inside that deadline.
	defaultThawRetry = 500 * time.Millisecond
)

// ─────────────────────────────────────────────────────────────────────
// FleetVolumeSnapshotter use case (D-080).
// ─────────────────────────────────────────────────────────────────────

// FleetVolumeSnapshotter takes GUEST-CONSISTENT snapshots of a resource's
// persistent volumes and records them as durable recovery points (D-080, CP4.5
// §9 #3). For an attached volume it quiesces the guest filesystem (syncfs +
// fsfreeze over the D-185a control channel), pauses the resident engine ONLY for
// the local sparse copy — the frozen/paused window never covers the (slower)
// upload — then resumes, thaws, and streams the copy to the artifact store.
//
// Consistency comes from the GUEST freeze, not from the pause: Firecracker's
// Pause stops vCPUs without flushing the guest kernel's dirty page cache
// (#p19-snapshot-not-guest-consistent). A guest that cannot be quiesced is
// REFUSED (ErrSnapshotNotQuiescible) — no recovery point, no SnapshotRef —
// because everything downstream treats a recovery point as trustworthy and the
// checksum gate proves only transport integrity, never source consistency.
type FleetVolumeSnapshotter struct {
	pauser   VMPauser
	guest    gateway.GuestControl
	recycler ReplicaRecycler
	store    ArtifactStore
	volumes  repository.VolumeRepository
	replicas repository.ReplicaRepository
	recovery repository.FleetResourceRepository

	// copyFile makes the local backing-file copy. It defaults to a sparse `cp`
	// and is overridable in tests (the real sparse cp is a coreutils call that a
	// non-Linux test host cannot run).
	copyFile func(src, dst string) error

	// Thaw budget. Fields (not constants) so tests drive the escalation path
	// without waiting on the real deadline.
	thawDeadline time.Duration
	thawRetry    time.Duration
}

// NewFleetVolumeSnapshotter constructs the use case. guest is the post-boot
// control channel every attached-volume snapshot quiesces through; recycler is
// the escalation path for a guest that will not thaw (nil leaves the guest's own
// dead-man auto-thaw as the only backstop).
func NewFleetVolumeSnapshotter(
	pauser VMPauser,
	guest gateway.GuestControl,
	recycler ReplicaRecycler,
	store ArtifactStore,
	volumes repository.VolumeRepository,
	replicas repository.ReplicaRepository,
	recovery repository.FleetResourceRepository,
) *FleetVolumeSnapshotter {
	return &FleetVolumeSnapshotter{
		pauser:       pauser,
		guest:        guest,
		recycler:     recycler,
		store:        store,
		volumes:      volumes,
		replicas:     replicas,
		recovery:     recovery,
		copyFile:     realSparseCopy,
		thawDeadline: defaultThawDeadline,
		thawRetry:    defaultThawRetry,
	}
}

// SnapshotAppVolumes snapshots every persistent volume of the app backing a
// resource and returns the recovery points it created. It aborts on the first
// volume that fails (a partial snapshot of a data resource is not a recovery
// point the caller can trust).
func (s *FleetVolumeSnapshotter) SnapshotAppVolumes(ctx context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	vols, err := s.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	points := make([]domain.FleetResourceRecoveryPoint, 0, len(vols))
	for i := range vols {
		rp, err := s.snapshotVolume(ctx, resourceID, &vols[i])
		if err != nil {
			return nil, err
		}
		points = append(points, rp)
	}
	return points, nil
}

// snapshotVolume snapshots one volume. Attached: quiesce the guest (syncfs +
// freeze) → pause the VMM → flush the host fd → local sparse copy → resume →
// thaw → upload → recovery-point row → SnapshotRef. A guest that cannot be
// quiesced, and an upload that fails, both abort with NO recovery-point row.
func (s *FleetVolumeSnapshotter) snapshotVolume(ctx context.Context, resourceID uuid.UUID, vol *domain.Volume) (domain.FleetResourceRecoveryPoint, error) {
	var zero domain.FleetResourceRecoveryPoint
	if vol.BackingPath == "" {
		return zero, fmt.Errorf("volume %s has no backing file to snapshot", vol.ID)
	}
	snapshotID := uuid.New()
	tmpPath := filepath.Join(filepath.Dir(vol.BackingPath), "."+snapshotID.String()+".snap.tmp")

	if vol.AttachedReplica != nil {
		replica, err := s.replicas.FindByID(ctx, *vol.AttachedReplica)
		if err != nil {
			return zero, fmt.Errorf("load attached replica: %w", err)
		}
		// Quiesce the GUEST first. Everything after this runs against a filesystem
		// the guest kernel has flushed and frozen; without it the copy below can
		// miss or tear writes the guest already acked.
		if err := s.quiesce(ctx, replica); err != nil {
			return zero, err
		}
		// From here the guest filesystem is FROZEN — every exit path must thaw it.
		if err := s.pauser.Pause(ctx, replica.SocketPath); err != nil {
			s.thawOrEscalate(ctx, replica)
			return zero, fmt.Errorf("pause vm for snapshot: %w", err)
		}
		// The pause is NOT redundant with the freeze, and must not be deleted as
		// such: a frozen filesystem cannot change, but the auto-thaw dead-man the
		// guest armed at Freeze is a timer INSIDE the guest, and pausing the vCPUs
		// suspends it. That is what makes a copy of an arbitrarily large volume
		// unable to race the guest thawing itself mid-copy.
		//
		// The pause window covers ONLY the local copy. Resume runs in a defer so a
		// copy failure never leaves the engine paused (mirrors resumeQuietly).
		copyErr := func() error {
			defer s.resumeQuietly(ctx, replica.SocketPath, replica.ID)
			// Flush the HOST's own dirty pages for the backing file before copying
			// it — the design's mandated park-capture ordering
			// (docs/designs/sentiae-database-platform.md §4.2). It is close to a
			// no-op today because `cp` reads back through that same page cache, but
			// it becomes load-bearing the moment the snapshot is taken BELOW the
			// cache (a ZFS dataset snapshot in Phase 2 captures the block layer
			// only, and "memory believes a write the snapshot lacks" is the
			// silent-corruption direction).
			if err := syncHostFile(vol.BackingPath); err != nil {
				return err
			}
			return s.copyFile(vol.BackingPath, tmpPath)
		}()
		// Thaw AFTER the resume: a paused guest cannot answer the control channel.
		s.thawOrEscalate(ctx, replica)
		if copyErr != nil {
			_ = os.Remove(tmpPath)
			return zero, fmt.Errorf("snapshot copy: %w", copyErr)
		}
	} else {
		// No attached replica: nothing is writing, so there is no guest to quiesce.
		// This copy is only as consistent as the last STOP of the engine that wrote
		// it was clean — which today it never is, because a resident stop is a VMM
		// kill (#resident-stop-is-vmm-kill). Making the detached branch trustworthy
		// depends on that fix, not on this one.
		if err := s.copyFile(vol.BackingPath, tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return zero, fmt.Errorf("snapshot copy: %w", err)
		}
	}
	defer func() { _ = os.Remove(tmpPath) }()

	size, err := fileSize(tmpPath)
	if err != nil {
		return zero, fmt.Errorf("stat snapshot copy: %w", err)
	}

	objectKey := fmt.Sprintf("volumes/%s/%s.ext4", vol.ID, snapshotID)
	checksum, err := uploadSnapshotFileHashed(s.store, objectKey, tmpPath)
	if err != nil {
		// A local-only copy is not a recovery point: abort with no catalog row.
		return zero, fmt.Errorf("upload snapshot: %w", err)
	}

	now := time.Now().UTC()
	volID := vol.ID
	rp := domain.FleetResourceRecoveryPoint{
		ID:         snapshotID,
		ResourceID: resourceID,
		VolumeID:   &volID,
		ObjectKey:  objectKey,
		Kind:       "snapshot",
		SizeBytes:  size,
		Checksum:   checksum,
		Verified:   false,
		CreatedAt:  now,
	}
	if err := s.recovery.SaveRecoveryPoint(ctx, &rp); err != nil {
		return zero, fmt.Errorf("save recovery point: %w", err)
	}

	vol.SnapshotRef = objectKey
	vol.UpdatedAt = now
	if err := s.volumes.Update(ctx, vol); err != nil {
		return zero, fmt.Errorf("update volume snapshot ref: %w", err)
	}
	return rp, nil
}

// quiesce flushes and then freezes the guest's data filesystem over the D-185a
// control channel. Any failure refuses the snapshot (ErrSnapshotNotQuiescible):
// a copy of an unfrozen guest may be torn, and a torn copy recorded as a
// recovery point is indistinguishable from a good one forever after.
func (s *FleetVolumeSnapshotter) quiesce(ctx context.Context, replica *domain.Replica) error {
	if s.guest == nil {
		return quiesceRefusal(replica, "reach", domain.ErrGuestControlUnavailable)
	}
	if err := s.guest.SyncFS(ctx, replica.SocketPath); err != nil {
		return quiesceRefusal(replica, "flush", err)
	}
	if err := s.guest.Freeze(ctx, replica.SocketPath); err != nil {
		// The guest arms its dead-man only on a freeze it completed, so a REFUSED
		// freeze left nothing frozen — but a freeze whose response was lost looks
		// identical from here and did leave the guest frozen. One best-effort thaw
		// (benign on an unfrozen filesystem) collapses that window instead of
		// waiting out the dead-man, which stays the backstop.
		if terr := s.guest.Thaw(ctx, replica.SocketPath); terr != nil {
			logger.FromContext(ctx).Warn("fleet snapshot: thaw after failed freeze",
				"replica_id", replica.ID, "err", terr)
		}
		return quiesceRefusal(replica, "freeze", err)
	}
	return nil
}

// quiesceRefusal wraps a control-channel failure in the refusal sentinel. The
// unavailable-channel case gets its own text because it has one likely cause an
// operator can act on immediately: the VM predates the control channel (D-185a)
// and has never had one, so no retry will ever succeed — only a reboot will.
func quiesceRefusal(replica *domain.Replica, op string, cause error) error {
	if errors.Is(cause, domain.ErrGuestControlUnavailable) {
		return fmt.Errorf("%w: replica %s has NO guest control channel — this VM was booted BEFORE the channel existed (D-185a), so it can never be quiesced as it stands and no retry will help; reboot the replica (it comes back with a channel) and snapshot again: %w",
			domain.ErrSnapshotNotQuiescible, replica.ID, cause)
	}
	return fmt.Errorf("%w: could not %s the guest filesystem of replica %s: %w",
		domain.ErrSnapshotNotQuiescible, op, replica.ID, cause)
}

// thawOrEscalate releases the freeze, retrying inside a bounded deadline.
//
// A thaw failure never fails the snapshot — the copy is already consistent by
// the time this runs — but it must not leave guest writers blocked either. When
// the retries are exhausted the replica is KILLED, and the reconciler boots a
// replacement on the same host and backing file within a tick. The guest's own
// dead-man auto-thaw is the backstop; this escalation is for the guest that is
// not honouring it.
func (s *FleetVolumeSnapshotter) thawOrEscalate(ctx context.Context, replica *domain.Replica) {
	lastErr := s.thawWithRetries(ctx, replica)
	if lastErr == nil {
		return
	}
	log := logger.FromContext(ctx)
	log.Error("fleet snapshot: guest filesystem NOT thawed after retries — killing the replica so its writers are not left blocked",
		"replica_id", replica.ID, "thaw_deadline", s.thawDeadline.String(), "err", lastErr)
	if s.recycler == nil {
		log.Error("fleet snapshot: no replica recycler wired — the guest stays frozen until its own dead-man auto-thaw fires",
			"replica_id", replica.ID)
		return
	}
	if err := s.recycler.DecommissionReplica(ctx, replica.ID); err != nil {
		log.Error("fleet snapshot: kill wedged replica", "replica_id", replica.ID, "err", err)
	}
}

// thawWithRetries thaws until it succeeds, the deadline passes, or ctx ends,
// returning the last failure (nil on success).
func (s *FleetVolumeSnapshotter) thawWithRetries(ctx context.Context, replica *domain.Replica) error {
	if s.guest == nil {
		// Unreachable: quiesce already refused a snapshot with no control channel.
		return domain.ErrGuestControlUnavailable
	}
	deadline := time.Now().Add(s.thawDeadline)
	for {
		err := s.guest.Thaw(ctx, replica.SocketPath)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.thawRetry):
		}
	}
}

// syncHostFile fsyncs the host's view of a file. Opening read-only is enough:
// fsync(2) flushes the inode's dirty pages regardless of the descriptor's access
// mode, and read-only is the weaker handle to take on a live VM's disk.
func syncHostFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open backing file %s to flush: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync backing file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close backing file %s: %w", path, err)
	}
	return nil
}

// resumeQuietly resumes a paused VM after the local copy, logging (but not
// propagating) a resume error — the copy is already done, and a failed resume is
// surfaced by the resident's own health, not by aborting the snapshot.
func (s *FleetVolumeSnapshotter) resumeQuietly(ctx context.Context, socketPath string, replicaID uuid.UUID) {
	if err := s.pauser.Resume(ctx, socketPath); err != nil {
		logger.FromContext(ctx).Warn("fleet snapshot: resume after copy", "replica_id", replicaID, "err", err)
	}
}

// realSparseCopy makes a sparse copy of a backing file (holes preserved so a
// large-but-empty volume copies cheaply). Linux/coreutils only — overridden in
// tests on hosts without GNU cp.
func realSparseCopy(src, dst string) error {
	out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp --sparse=always %s %s: %s: %w", src, dst, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// fileSize returns the on-disk size of a file in bytes.
func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
