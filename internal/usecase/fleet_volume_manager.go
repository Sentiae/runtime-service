package usecase

import (
	"context"
	"errors"
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

// VolumeEnsureMode declares what an ensure MEANS, because the backend cannot
// infer it and must never guess.
//
// ⚠ This is a data-loss control. The backend sees a directory and a path, never
// the ledger, so "the file is absent" is ambiguous to it: on a first provision
// that means "make it", and on an attach it means "the data this ledger row
// promises is GONE". Inferring create-if-absent from the filesystem alone turned
// the second case into the first — a lost fleet host minted a brand-new empty
// filesystem for every surviving fleet_volumes row and reported the deploy
// healthy, i.e. total data loss presented as success. The caller holds the
// ledger, so the caller declares the intent and the backend only obeys.
type VolumeEnsureMode string

const (
	// VolumeEnsureCreate is a FIRST provision: the ledger has no volume for this
	// (app, mount), so there is no data to lose and the backing file is made.
	VolumeEnsureCreate VolumeEnsureMode = "create"
	// VolumeEnsureAdopt is an attach/reboot/re-provision of a volume the ledger
	// already records: the file must already exist and is returned untouched. A
	// missing file is a hard failure (ErrVolumeBackingFileMissing), never a
	// reason to format a fresh one.
	VolumeEnsureAdopt VolumeEnsureMode = "adopt"
)

// VolumeEnsureInput is the wire-agnostic backing-file request. Mode is required:
// there is no safe default (see VolumeEnsureMode).
type VolumeEnsureInput struct {
	VolumeID uuid.UUID
	SizeMB   int64
	Dir      string
	Mode     VolumeEnsureMode
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
	// resources is the claim ledger the deletion seam consults to prove a
	// claim-owned volume's owner is retired before any byte is reclaimed (D-203).
	resources repository.FleetResourceRepository
}

// NewFleetVolumeManager constructs the use case. dir is the root under which
// per-volume ext4 backing files are materialized. resources is the claim ledger
// the deletion guard reads (D-203).
func NewFleetVolumeManager(volumes repository.VolumeRepository, backend VolumeBackend, dir string,
	resources repository.FleetResourceRepository) *FleetVolumeManager {
	return &FleetVolumeManager{volumes: volumes, backend: backend, dir: dir, resources: resources}
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
			// ADOPT — the ledger already carries this volume, and a row only ever
			// exists because a materialize once succeeded. So the file is the
			// customer's data: re-attach it, and if it is not on this host REFUSE.
			// Creating here would hand a re-provision (or a reboot after the host
			// lost its disk) a brand-new empty filesystem under the same row.
			out, berr := m.backend.Ensure(ctx, VolumeEnsureInput{VolumeID: vol.ID, SizeMB: vol.SizeMB, Dir: m.dir, Mode: VolumeEnsureAdopt})
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
		// CREATE — no ledger row for this (app, mount), so this is the volume's
		// first materialization and there is no data that could be destroyed. The
		// spec's id is NOT evidence of a prior volume: the ledger is keyed by
		// (app, mount) and the wire id is caller-supplied (an unparseable one is
		// replaced with a fresh uuid at the boundary).
		out, berr := m.backend.Ensure(ctx, VolumeEnsureInput{VolumeID: id, SizeMB: spec.SizeMB, Dir: m.dir, Mode: VolumeEnsureCreate})
		if berr != nil {
			return nil, fmt.Errorf("ensure backing file: %w", berr)
		}
		app := appID
		vol := &domain.Volume{
			ID:          id,
			AppID:       &app,
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
// decommissioned: it removes each on-host ext4 backing file, then deletes the
// volume rows. Nothing cascades (0024): rows AND files are reclaimed here, and
// only here. Per-file delete errors are logged and the loop
// continues so one failure never strands the rest; the first error is returned
// after every backing file has been attempted so the caller can surface it.
// This must NOT run on a replica restart (that would destroy persisted data) —
// only DecommissionApp calls it, never DecommissionReplica.
func (m *FleetVolumeManager) DeleteAppVolumes(ctx context.Context, appID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	// D-203: a volume a LIVE claim owns is deletable only through the resource's
	// own snapshot-first teardown (which stamps decommissioned_at BEFORE calling
	// down — that stamp is what lets the legitimate path pass here). Fail closed
	// on every uncertainty: an unwired ledger, an unreadable one, and an owner
	// row that does not exist at all all refuse.
	// The pre-pass runs before ANY file or row is touched: a guard that refuses
	// after the first unlink is worthless.
	for i := range vols {
		if vols[i].ResourceID == nil {
			continue
		}
		if m.resources == nil {
			return fmt.Errorf("%w: volume %s is owned by resource %s and no claim ledger is wired to prove the claim retired",
				domain.ErrVolumeOwnedByLiveResource, vols[i].ID, *vols[i].ResourceID)
		}
		res, rerr := m.resources.GetResourceByHandle(ctx, *vols[i].ResourceID)
		if rerr != nil {
			if errors.Is(rerr, domain.ErrResourceNotFound) {
				return fmt.Errorf("%w: volume %s names owner resource %s but no such row exists — the 0024 FK makes this state impossible except by manual surgery; refusing to reclaim",
					domain.ErrVolumeOwnedByLiveResource, vols[i].ID, *vols[i].ResourceID)
			}
			return fmt.Errorf("prove volume %s's owning claim retired: %w", vols[i].ID, rerr)
		}
		if res.DecommissionedAt == nil {
			return fmt.Errorf("%w: volume %s is owned by live %s/%s resource %s — decommission the RESOURCE, which snapshots first",
				domain.ErrVolumeOwnedByLiveResource, vols[i].ID, res.Class, res.Tier, res.ID)
		}
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

// BindToResource stamps claim ownership onto the app's volumes, write-once
// (mirrors BindToHost): a volume already owned by resourceID is left alone;
// one owned by a DIFFERENT resource refuses with ErrVolumeClaimConflict —
// silently re-parenting a customer's bytes is never an upsert.
func (m *FleetVolumeManager) BindToResource(ctx context.Context, appID, resourceID uuid.UUID) error {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	now := time.Now().UTC()
	for i := range vols {
		if vols[i].ResourceID != nil {
			if *vols[i].ResourceID != resourceID {
				return fmt.Errorf("volume %s: %w (owned by %s, asked %s)",
					vols[i].ID, domain.ErrVolumeClaimConflict, *vols[i].ResourceID, resourceID)
			}
			continue
		}
		res := resourceID
		vols[i].ResourceID = &res
		vols[i].UpdatedAt = now
		if err := m.volumes.Update(ctx, &vols[i]); err != nil {
			return fmt.Errorf("bind volume to resource claim: %w", err)
		}
	}
	return nil
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
