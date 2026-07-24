package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// defaultVolumeMount is the single, hardcoded guest mount path for a persistent
// volume this cycle (the proto carries no mount_path — see rt#9 scope).
const defaultVolumeMount = "/data"

// ─────────────────────────────────────────────────────────────────────
// VolumeBackend port — materializes + removes the ext4 backing file a
// persistent volume attaches to (implemented under internal/infrastructure/
// volume; fail-loud off the firecracker host).
// ─────────────────────────────────────────────────────────────────────

// VolumeBackend materializes the durable ext4 backing file a volume is attached
// to and removes it on delete. Ensure is idempotent.
type VolumeBackend interface {
	Ensure(ctx context.Context, in VolumeEnsureInput) (VolumeEnsureOutput, error)
	Delete(ctx context.Context, backingPath string) error
}

// VolumeEnsureInput is the wire-agnostic backing-file request.
type VolumeEnsureInput struct {
	VolumeID uuid.UUID
	SizeMB   int64
	Dir      string
}

// VolumeEnsureOutput is the backing-file result.
type VolumeEnsureOutput struct {
	BackingPath string
}

// FailLoudVolumeBackend is wired when the executor is not firecracker. Every call
// fails with ErrVolumeBackendUnavailable so a volume is never silently faked on a
// host without KVM (mirrors FailLoudImageBooter).
type FailLoudVolumeBackend struct{}

func (FailLoudVolumeBackend) Ensure(context.Context, VolumeEnsureInput) (VolumeEnsureOutput, error) {
	return VolumeEnsureOutput{}, domain.ErrVolumeBackendUnavailable
}
func (FailLoudVolumeBackend) Delete(context.Context, string) error {
	return domain.ErrVolumeBackendUnavailable
}

// ─────────────────────────────────────────────────────────────────────
// VolumeSpecInput — one requested volume from the deployment descriptor.
// ─────────────────────────────────────────────────────────────────────

// VolumeSpecInput is one requested volume (wire-agnostic).
type VolumeSpecInput struct {
	ID        uuid.UUID
	SizeMB    int64
	MountPath string
}

// ─────────────────────────────────────────────────────────────────────
// FleetVolumeManager use case.
// ─────────────────────────────────────────────────────────────────────

// FleetVolumeManager owns the durable persistent-volume lifecycle for a fleet
// app (runtime-fleet CP4 rt#9): materialize backing files, pin a volume-bearing
// app to a host (write-once affinity), and attach/detach a volume to the resident
// replica that holds it. Single-writer: at most one replica attaches at a time.
type FleetVolumeManager struct {
	volumes repository.VolumeRepository
	backend VolumeBackend
	dir     string
}

// NewFleetVolumeManager constructs the use case. dir is the root under which
// per-volume ext4 backing files are materialized.
func NewFleetVolumeManager(volumes repository.VolumeRepository, backend VolumeBackend, dir string) *FleetVolumeManager {
	return &FleetVolumeManager{volumes: volumes, backend: backend, dir: dir}
}

// EnsureAppVolumes upserts a domain.Volume per spec (keyed by app + mount path),
// materializes each backing file, and persists the backing path. Idempotent per
// (appID, mountPath): a re-provision reuses the existing volume + backing file.
func (m *FleetVolumeManager) EnsureAppVolumes(ctx context.Context, appID uuid.UUID, specs []VolumeSpecInput) ([]domain.Volume, error) {
	existing, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	byMount := make(map[string]*domain.Volume, len(existing))
	for i := range existing {
		byMount[existing[i].MountPath] = &existing[i]
	}

	result := make([]domain.Volume, 0, len(specs))
	for _, spec := range specs {
		mount := spec.MountPath
		if mount == "" {
			mount = defaultVolumeMount
		}
		now := time.Now().UTC()

		if vol := byMount[mount]; vol != nil {
			// Idempotent: re-ensure the backing file (no-op if it already exists)
			// and record any change to the backing path.
			out, berr := m.backend.Ensure(ctx, VolumeEnsureInput{VolumeID: vol.ID, SizeMB: vol.SizeMB, Dir: m.dir})
			if berr != nil {
				return nil, fmt.Errorf("ensure backing file: %w", berr)
			}
			if vol.BackingPath != out.BackingPath {
				vol.BackingPath = out.BackingPath
				vol.UpdatedAt = now
				if err := m.volumes.Update(ctx, vol); err != nil {
					return nil, fmt.Errorf("update volume: %w", err)
				}
			}
			result = append(result, *vol)
			continue
		}

		id := spec.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		out, berr := m.backend.Ensure(ctx, VolumeEnsureInput{VolumeID: id, SizeMB: spec.SizeMB, Dir: m.dir})
		if berr != nil {
			return nil, fmt.Errorf("ensure backing file: %w", berr)
		}
		vol := &domain.Volume{
			ID:          id,
			AppID:       appID,
			SizeMB:      spec.SizeMB,
			MountPath:   mount,
			BackingPath: out.BackingPath,
			Status:      domain.VolumeStatusAvailable,
			DeviceName:  "/dev/vdb",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := m.volumes.Create(ctx, vol); err != nil {
			return nil, fmt.Errorf("create volume: %w", err)
		}
		byMount[mount] = vol
		result = append(result, *vol)
	}
	return result, nil
}

// DeleteAppVolumes reclaims an app's persistent volumes when the APP is fully
// decommissioned: it removes each on-host ext4 backing file (nothing else frees
// it — the fleet_apps row cascade deletes only the fleet_volumes rows), then
// deletes the volume rows. Per-file delete errors are logged and the loop
// continues so one failure never strands the rest; the first error is returned
// after every backing file has been attempted so the caller can surface it.
// This must NOT run on a replica restart (that would destroy persisted data) —
// only DecommissionApp calls it, never DecommissionReplica.
func (m *FleetVolumeManager) DeleteAppVolumes(ctx context.Context, appID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	var firstErr error
	for i := range vols {
		if vols[i].BackingPath != "" {
			if derr := m.backend.Delete(ctx, vols[i].BackingPath); derr != nil {
				logger.FromContext(ctx).Warn("fleet volume: delete backing file",
					"app_id", appID, "volume_id", vols[i].ID, "backing_path", vols[i].BackingPath, "err", derr)
				if firstErr == nil {
					firstErr = fmt.Errorf("delete backing file: %w", derr)
				}
				continue
			}
		}
		if derr := m.volumes.Delete(ctx, vols[i].ID); derr != nil {
			logger.FromContext(ctx).Warn("fleet volume: delete volume row",
				"app_id", appID, "volume_id", vols[i].ID, "err", derr)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete volume row: %w", derr)
			}
		}
	}
	return firstErr
}

// HasVolumes reports whether the app has any persistent volume.
func (m *FleetVolumeManager) HasVolumes(ctx context.Context, appID uuid.UUID) (bool, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return false, fmt.Errorf("list volumes: %w", err)
	}
	return len(vols) > 0, nil
}

// PrimaryVolume returns the app's first (data) volume. The bool reports whether
// the app has any volume.
func (m *FleetVolumeManager) PrimaryVolume(ctx context.Context, appID uuid.UUID) (*domain.Volume, bool, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, false, fmt.Errorf("list volumes: %w", err)
	}
	if len(vols) == 0 {
		return nil, false, nil
	}
	v := vols[0]
	return &v, true, nil
}

// AffinityHost returns the host a volume-bearing app is pinned to. The bool is
// true when any of the app's volumes carries a host_affinity (authoritative);
// (nil,false) otherwise.
func (m *FleetVolumeManager) AffinityHost(ctx context.Context, appID uuid.UUID) (*uuid.UUID, bool, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, false, fmt.Errorf("list volumes: %w", err)
	}
	for i := range vols {
		if vols[i].HostAffinity != nil {
			host := *vols[i].HostAffinity
			return &host, true, nil
		}
	}
	return nil, false, nil
}

// BindToHost pins the app's volumes to a host, write-once: a volume that already
// carries a host_affinity is never re-pinned (its data lives on that host).
func (m *FleetVolumeManager) BindToHost(ctx context.Context, appID, hostID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	now := time.Now().UTC()
	for i := range vols {
		if vols[i].HostAffinity != nil {
			continue
		}
		host := hostID
		vols[i].HostAffinity = &host
		vols[i].UpdatedAt = now
		if err := m.volumes.Update(ctx, &vols[i]); err != nil {
			return fmt.Errorf("bind volume to host: %w", err)
		}
	}
	return nil
}

// AttachTo marks the app's volumes attached to a replica (single-writer).
func (m *FleetVolumeManager) AttachTo(ctx context.Context, appID, replicaID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	now := time.Now().UTC()
	for i := range vols {
		rep := replicaID
		vols[i].AttachedReplica = &rep
		vols[i].Status = domain.VolumeStatusAttached
		vols[i].UpdatedAt = now
		if err := m.volumes.Update(ctx, &vols[i]); err != nil {
			return fmt.Errorf("attach volume: %w", err)
		}
	}
	return nil
}

// DetachFrom clears the app's volume attachment (status back to available).
func (m *FleetVolumeManager) DetachFrom(ctx context.Context, appID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	now := time.Now().UTC()
	for i := range vols {
		vols[i].AttachedReplica = nil
		// A degraded volume stays degraded — detach never revives it this cycle.
		// A RESTORING volume likewise: the restore drains the replica itself
		// (ScaleApp 0 → DecommissionReplica → here), so reviving the status on
		// detach would tear down the very stand-off that keeps the reconciler and
		// the activator off the backing file until the swap lands (D-184).
		if vols[i].Status != domain.VolumeStatusDegraded && vols[i].Status != domain.VolumeStatusRestoring {
			vols[i].Status = domain.VolumeStatusAvailable
		}
		vols[i].UpdatedAt = now
		if err := m.volumes.Update(ctx, &vols[i]); err != nil {
			return fmt.Errorf("detach volume: %w", err)
		}
	}
	return nil
}

// MarkDegraded marks the app's volumes degraded (affinity host gone). Terminal
// this cycle — there is no cross-host restore.
func (m *FleetVolumeManager) MarkDegraded(ctx context.Context, appID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	now := time.Now().UTC()
	for i := range vols {
		vols[i].Status = domain.VolumeStatusDegraded
		vols[i].UpdatedAt = now
		if err := m.volumes.Update(ctx, &vols[i]); err != nil {
			return fmt.Errorf("mark volume degraded: %w", err)
		}
	}
	return nil
}

// IsDegraded reports whether any of the app's volumes is degraded.
func (m *FleetVolumeManager) IsDegraded(ctx context.Context, appID uuid.UUID) (bool, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return false, fmt.Errorf("list volumes: %w", err)
	}
	for i := range vols {
		if vols[i].Status == domain.VolumeStatusDegraded {
			return true, nil
		}
	}
	return false, nil
}
