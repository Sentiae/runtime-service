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
	// Created reports that THIS invocation brought the file into existence and
	// formatted it. It is the compensation seam's only licence to delete: a file
	// this call did not create is either a pre-existing customer volume or another
	// caller's, and unlinking it to tidy up after a failed row insert would be the
	// data-loss the whole saga is ordered to avoid. Validated pre-existing files
	// (both modes) report false.
	Created bool
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

	// selfHost is this process's durable fleet host identity, and it is what makes
	// host_affinity MEAN something (#fleet-reconciler-acts-on-foreign-host-
	// replicas). Immutable and constructor-required: a manager that could be
	// re-scoped, or that defaulted to "no host", would be a manager that can adopt,
	// attach, snapshot or delete bytes sitting on a filesystem it does not have.
	selfHost uuid.UUID
}

// NewFleetVolumeManager constructs the use case. dir is the root under which
// per-volume ext4 backing files are materialized. resources is the claim ledger
// the deletion guard reads (D-203). selfHost is this instance's durable fleet
// host id — REQUIRED, because every method below is an authority claim over
// bytes that exist on exactly one machine.
func NewFleetVolumeManager(volumes repository.VolumeRepository, backend VolumeBackend, dir string,
	resources repository.FleetResourceRepository, selfHost uuid.UUID) (*FleetVolumeManager, error) {
	if selfHost == uuid.Nil {
		// Refused rather than defaulted: with a nil self host every affinity
		// comparison below would fail closed for OWNED rows and the manager would be
		// useless, or (if it were made permissive) would act on every host's bytes.
		return nil, fmt.Errorf("%w: a volume manager needs this instance's fleet host identity before it may touch any backing file",
			domain.ErrVolumeHostMismatch)
	}
	return &FleetVolumeManager{volumes: volumes, backend: backend, dir: dir, resources: resources, selfHost: selfHost}, nil
}

// ownedVolumes lists an app's volumes and proves EVERY one of them is pinned to
// this host before the caller performs any write or local side effect.
//
// It returns the rows so the caller does not list a second time: a re-list is a
// second point in time, and a fence that checks one snapshot while the method
// mutates another is not a fence. Zero volumes passes vacuously — a stateless
// app has nothing on any host — so callers keep their existing no-op behavior.
//
// A nil affinity is refused exactly like a foreign one. An unstamped row proves
// nothing about where its bytes are, and the whole point of the fence is that
// only positive evidence authorizes a local side effect.
func (m *FleetVolumeManager) ownedVolumes(ctx context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	for i := range vols {
		if vols[i].HostAffinity == nil {
			return nil, fmt.Errorf("%w: volume %s carries no host affinity, so this host cannot prove its data is here",
				domain.ErrVolumeHostMismatch, vols[i].ID)
		}
		if *vols[i].HostAffinity != m.selfHost {
			return nil, fmt.Errorf("%w: volume %s is pinned to another fleet host",
				domain.ErrVolumeHostMismatch, vols[i].ID)
		}
	}
	return vols, nil
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
			//
			// The affinity is inspected BEFORE the backend is touched: a foreign row
			// must cost zero backend and zero repository calls, so that "this host did
			// nothing" is a property of the code path and not of what the backend
			// happened to answer.
			if vol.HostAffinity != nil && *vol.HostAffinity != m.selfHost {
				return nil, fmt.Errorf("%w: volume %s at %s is pinned to another fleet host",
					domain.ErrVolumeHostMismatch, vol.ID, mount)
			}
			legacy := vol.HostAffinity == nil
			// Adopt mode on BOTH branches, including the legacy nil-affinity one. A
			// successfully VALIDATED local file is the only evidence that this host may
			// claim a row that names no host; a missing file returns
			// ErrVolumeBackingFileMissing and binds nothing, because "the file is not
			// here" must never become "so I will make one and call it mine".
			out, berr := m.backend.Ensure(ctx, VolumeEnsureInput{VolumeID: vol.ID, SizeMB: vol.SizeMB, Dir: m.dir, Mode: VolumeEnsureAdopt})
			if berr != nil {
				return nil, fmt.Errorf("ensure backing file: %w", berr)
			}
			if vol.BackingPath == "" {
				return nil, fmt.Errorf("%w: volume %s", domain.ErrVolumeBackingPathUnset, vol.ID)
			}
			// No broad Save here, deliberately: writing the whole row back would race
			// the affinity CAS below and could stamp a stale HostAffinity over the
			// winner's. The path is a pure function of (dir, volume id) on both sides,
			// so a divergence is a wiring fault to surface, never a field to repair.
			if vol.BackingPath != out.BackingPath {
				return nil, fmt.Errorf("volume %s: the adopted backing file %s is not the path the ledger records (%s) — this host's volume directory does not match the row",
					vol.ID, out.BackingPath, vol.BackingPath)
			}
			if legacy {
				// The atomic CAS, not a read-then-Save: two hosts adopting the same
				// legacy row concurrently must produce exactly one winner, and the LOSER
				// must not delete anything. It did not create those bytes.
				res, cerr := m.volumes.BindHostAffinity(ctx, vol.ID, m.selfHost)
				if cerr != nil {
					return nil, fmt.Errorf("bind volume %s host affinity: %w", vol.ID, cerr)
				}
				if res.Outcome == repository.VolumeHostBindConflict {
					return nil, fmt.Errorf("%w: volume %s was bound to another fleet host while this host was adopting it",
						domain.ErrVolumeHostMismatch, vol.ID)
				}
				self := m.selfHost
				vol.HostAffinity = &self
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
		self := m.selfHost
		vol := &domain.Volume{
			ID:    id,
			AppID: &app,
			// The affinity is written INTO the first insert, never stamped afterwards:
			// the bytes were just materialized here, so there is no moment at which a
			// persisted row exists without naming the host that holds them. It is also
			// what makes a stateful app's later scheduling deterministic — it can only
			// be placed back on the host that has its disk.
			HostAffinity: &self,
			SizeMB:       spec.SizeMB,
			MountPath:    mount,
			BackingPath:  out.BackingPath,
			Status:       domain.VolumeStatusAvailable,
			DeviceName:   "/dev/vdb",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := m.volumes.Create(ctx, vol); err != nil {
			// A filesystem/DB saga, not a transaction: the bytes exist and the row does
			// not. Every branch below is decided by AUTHORITATIVE evidence, and every
			// uncertainty resolves toward keeping the file — an unattributed file is a
			// report-only ledger-reconciler finding, whereas a wrongly deleted one is a
			// customer's data.
			return nil, m.compensateCreate(ctx, appID, mount, vol, out, err)
		}
		byMount[mount] = vol
		result = append(result, *vol)
	}
	return result, nil
}

// compensateCreate decides what to do with the file a failed volume-row insert
// left behind, and returns the error EnsureAppVolumes reports.
//
// It never deletes a file this attempt did not create (out.Created == false),
// and it never deletes on an uncertain read: the re-list is the only authority
// on who owns the mount path, so a re-list that fails means ownership is
// unknown, and unknown ownership is never permission to unlink possible customer
// bytes.
func (m *FleetVolumeManager) compensateCreate(ctx context.Context, appID uuid.UUID, mount string,
	attempt *domain.Volume, out VolumeEnsureOutput, cause error) error {
	// Authoritative re-read: did a concurrent provision commit a row for this
	// (app, mount) while this attempt was losing?
	after, lerr := m.volumes.ListByApp(ctx, appID)
	if lerr != nil {
		// Ownership is now UNKNOWN. Retain the file and report; the report-only
		// ledger reconciler surfaces an unattributed file, and no path here may
		// trade that for a possible deletion of live data.
		logger.FromContext(ctx).Error("fleet volume: create failed and the authoritative re-read failed too — the materialized backing file is RETAINED because nothing can prove who owns it",
			"app_id", appID, "volume_id", attempt.ID, "backing_path", attempt.BackingPath, "err", cause, "reread_err", lerr)
		return fmt.Errorf("create volume: %w (ownership of %s could not be re-read: %v)", cause, attempt.BackingPath, lerr)
	}
	var winner *domain.Volume
	for i := range after {
		if after[i].MountPath == mount {
			winner = &after[i]
			break
		}
	}
	switch {
	case winner == nil:
		// Proven: no row owns this path. The file is this attempt's residue and may
		// be reclaimed — but only if this attempt is what created it.
		m.deleteOwnAttempt(ctx, attempt, out)
		return fmt.Errorf("create volume: %w", cause)

	case winner.ID == attempt.ID && winner.BackingPath == attempt.BackingPath &&
		winner.HostAffinity != nil && *winner.HostAffinity == m.selfHost:
		// The insert actually landed (a lost ack, a retried write): the committed row
		// IS this attempt. Adopting it is correct and deleting the file would destroy
		// the volume the ledger now promises.
		logger.FromContext(ctx).Warn("fleet volume: create reported an error but the committed row is this attempt's — adopting it and keeping the backing file",
			"app_id", appID, "volume_id", winner.ID, "backing_path", winner.BackingPath, "err", cause)
		return nil

	default:
		// Someone else owns this mount, or owns it on another host. This attempt's
		// file is not the winner's, so it may be reclaimed — again only when this
		// attempt created it.
		m.deleteOwnAttempt(ctx, attempt, out)
		if winner.HostAffinity == nil || *winner.HostAffinity != m.selfHost {
			return fmt.Errorf("%w: volume %s at %s was committed on another fleet host while this host was materializing it",
				domain.ErrVolumeHostMismatch, winner.ID, mount)
		}
		return fmt.Errorf("create volume: %w (volume %s already holds mount %s)", cause, winner.ID, mount)
	}
}

// deleteOwnAttempt removes the backing file THIS attempt materialized, and only
// then. A Created=false path is a pre-existing file the create branch merely
// validated, and unlinking it would delete data this call did not produce.
func (m *FleetVolumeManager) deleteOwnAttempt(ctx context.Context, attempt *domain.Volume, out VolumeEnsureOutput) {
	if !out.Created || attempt.BackingPath == "" {
		return
	}
	if derr := m.backend.Delete(ctx, attempt.BackingPath); derr != nil {
		logger.FromContext(ctx).Warn("fleet volume: reclaim the backing file of a failed create",
			"volume_id", attempt.ID, "backing_path", attempt.BackingPath, "err", derr)
	}
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
	// The host fence runs FIRST, before the claim guard and before any unlink: a
	// host that does not hold the bytes cannot reclaim them, and a delete loop that
	// discovered that after the first unlink would already have destroyed data. It
	// also returns the rows, so the guard and the loop below judge exactly the
	// snapshot the fence approved.
	vols, err := m.ownedVolumes(ctx, appID)
	if err != nil {
		return err
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
	// Claim ownership may only be stamped by the host that holds the bytes: it is
	// the stamp the durability machinery later reads to decide WHERE a resource's
	// protection runs, so a foreign stamp would point the cadence at a filesystem
	// that has no such file. Preflighted before the atomic bind, all-or-nothing.
	if _, err := m.ownedVolumes(ctx, appID); err != nil {
		return err
	}
	res, err := m.volumes.BindVolumesToResource(ctx, appID, resourceID)
	if err != nil {
		return fmt.Errorf("bind volume to resource claim: %w", err)
	}
	if res.Outcome == repository.VolumeBindConflict {
		return fmt.Errorf("volume %s: %w (owned by %s, asked %s)",
			res.ConflictVolumeID, domain.ErrVolumeClaimConflict, res.ConflictOwner, resourceID)
	}
	return nil
}

// HasUnstampedVolumes reports whether the app still holds a volume that carries
// no claim owner. The repository error is propagated, never folded into false: a
// failed query must never be read as "stamped".
func (m *FleetVolumeManager) HasUnstampedVolumes(ctx context.Context, appID uuid.UUID) (bool, error) {
	unstamped, err := m.volumes.HasUnstampedVolumes(ctx, appID)
	if err != nil {
		return false, fmt.Errorf("check unstamped volumes: %w", err)
	}
	return unstamped, nil
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

// AffinityHost returns the host a volume-bearing app's data is pinned to. The
// bool is true when the app has volumes and they AGREE on one non-nil host.
//
// It is deliberately unanimous rather than first-match. It is the input the
// scheduler pins a stateful placement with, so an app whose volumes disagree —
// or one carrying an unstamped row — has no single answer, and returning the
// first non-nil one would place a replica on a host holding only part of its
// data. That refuses (ErrVolumeHostMismatch) instead of guessing.
func (m *FleetVolumeManager) AffinityHost(ctx context.Context, appID uuid.UUID) (*uuid.UUID, bool, error) {
	vols, err := m.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, false, fmt.Errorf("list volumes: %w", err)
	}
	if len(vols) == 0 {
		return nil, false, nil // a stateless app is pinned to nothing
	}
	var host *uuid.UUID
	for i := range vols {
		if vols[i].HostAffinity == nil {
			return nil, false, fmt.Errorf("%w: volume %s of app %s carries no host affinity, so the app's data has no provable location",
				domain.ErrVolumeHostMismatch, vols[i].ID, appID)
		}
		if host == nil {
			h := *vols[i].HostAffinity
			host = &h
			continue
		}
		if *vols[i].HostAffinity != *host {
			return nil, false, fmt.Errorf("%w: app %s has volumes pinned to DIFFERENT hosts, so no single host holds its data",
				domain.ErrVolumeHostMismatch, appID)
		}
	}
	return host, true, nil
}

// AttachTo marks the app's volumes attached to a replica (single-writer).
func (m *FleetVolumeManager) AttachTo(ctx context.Context, appID, replicaID uuid.UUID) error {
	// Attachment is a host-local claim on a host-local file: only the host holding
	// the bytes may declare a replica the single writer of them.
	vols, err := m.ownedVolumes(ctx, appID)
	if err != nil {
		return err
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
	// Fenced like AttachTo: releasing the single-writer claim is the other half of
	// the same host-local authority, and a foreign detach would tell the owning
	// host's live VM that nothing holds its disk.
	vols, err := m.ownedVolumes(ctx, appID)
	if err != nil {
		return err
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
//
// ⚠ DELIBERATELY NOT HOST-FENCED, and this exception must not be widened. It is
// a ledger-only availability ANNOTATION: it touches no byte, no guest, no
// attachment, no claim. Fencing it would be actively wrong — the condition it
// records is precisely "the host that holds this data is gone", so requiring
// that host to record it would mean the fact can only be written by a machine
// that cannot write it. Every byte, attach, detach, bind, snapshot and restore
// verb on this type IS fenced; this one alone is not.
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
