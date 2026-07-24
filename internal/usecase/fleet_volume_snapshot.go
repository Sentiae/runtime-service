package usecase

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
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
// freezes the guest so a copy of its data disk is crash-consistent; Resume
// unfreezes it. The firecracker Provider implements this.
type VMPauser interface {
	Pause(ctx context.Context, socketPath string) error
	Resume(ctx context.Context, socketPath string) error
}

// ─────────────────────────────────────────────────────────────────────
// FleetVolumeSnapshotter use case (D-080).
// ─────────────────────────────────────────────────────────────────────

// FleetVolumeSnapshotter takes crash-consistent snapshots of a resource's
// persistent volumes and records them as durable recovery points (D-080, CP4.5
// §9 #3). For an attached volume it pauses the resident engine ONLY for the
// local sparse copy — the paused window never covers the (slower) upload — then
// resumes and streams the copy to the artifact store. Crash-consistency is
// provided by the pause (a paused ext4 is a clean point-in-time); this slice
// does not issue a guest fsfreeze.
type FleetVolumeSnapshotter struct {
	pauser   VMPauser
	store    ArtifactStore
	volumes  repository.VolumeRepository
	replicas repository.ReplicaRepository
	recovery repository.FleetResourceRepository

	// copyFile makes the local backing-file copy. It defaults to a sparse `cp`
	// and is overridable in tests (the real sparse cp is a coreutils call that a
	// non-Linux test host cannot run).
	copyFile func(src, dst string) error
}

// NewFleetVolumeSnapshotter constructs the use case.
func NewFleetVolumeSnapshotter(
	pauser VMPauser,
	store ArtifactStore,
	volumes repository.VolumeRepository,
	replicas repository.ReplicaRepository,
	recovery repository.FleetResourceRepository,
) *FleetVolumeSnapshotter {
	return &FleetVolumeSnapshotter{
		pauser:   pauser,
		store:    store,
		volumes:  volumes,
		replicas: replicas,
		recovery: recovery,
		copyFile: realSparseCopy,
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

// snapshotVolume snapshots one volume: pause (if attached) → local sparse copy →
// resume → upload → recovery-point row → SnapshotRef. An upload failure aborts
// with NO recovery-point row.
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
		if err := s.pauser.Pause(ctx, replica.SocketPath); err != nil {
			return zero, fmt.Errorf("pause vm for snapshot: %w", err)
		}
		// The pause window covers ONLY the local copy. Resume runs in a defer so a
		// copy failure never leaves the engine frozen (mirrors resumeQuietly).
		copyErr := func() error {
			defer s.resumeQuietly(ctx, replica.SocketPath, replica.ID)
			return s.copyFile(vol.BackingPath, tmpPath)
		}()
		if copyErr != nil {
			_ = os.Remove(tmpPath)
			return zero, fmt.Errorf("snapshot copy: %w", copyErr)
		}
	} else {
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
