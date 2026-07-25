package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// FleetLedgerReconciler — the two-directional durability audit of the
// volume/recovery-point ledger against physical reality.
//
// The control plane and the host can disagree, and today nothing notices. A
// LIVE example: a fleet_volumes row advertising 20480 MB, status `available`,
// whose backing_path names a file that does not exist on its affinity host. The
// ledger promises 20 GB of customer data with zero bytes behind it, and every
// read of the control plane — capacity, health, the "you have a backup" answer —
// is wrong in the direction that loses data.
//
// ⚠ REPORT-ONLY, structurally. It holds READ-ONLY ports (see the Ledger*Reader
// interfaces below) and never touches the filesystem beyond os.ReadDir/os.Stat,
// so "it does not delete" is a property of its types rather than a promise in a
// comment. That is deliberate: every deletion in this subsystem is
// unrecoverable, and this reconciler's own oracles (the control-plane DB, the
// object store) are exactly the things that go down. An outage must never be
// read as "this data is orphaned" — so every uncertainty resolves to "cannot
// determine", never to "safe to remove". Acting on a divergence is a human
// decision informed by this report.
//
// It follows the fleet_staging_gc discipline: prove positively, treat every
// error (including a DB outage) as unprovable, log per entry and continue, never
// fail the sweep over one bad entry.
// ─────────────────────────────────────────────────────────────────────

// LedgerVolumeReader is the read-only slice of the volume ledger the reconciler
// needs. repository.VolumeRepository satisfies it; the narrow shape is the point
// — a report-only pass must not be able to Update or Delete a row even by
// mistake.
type LedgerVolumeReader interface {
	ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Volume, error)
}

// LedgerResourceReader is the read-only slice of the P19 resource ledger:
// app → live resource claim, and a resource's recovery points.
// repository.FleetResourceRepository satisfies it.
type LedgerResourceReader interface {
	FindLiveResourceByApp(ctx context.Context, appID uuid.UUID) (*domain.FleetResource, error)
	ListRecoveryPoints(ctx context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error)
}

// LedgerAppReader resolves an app row, which is where a volume's owning org
// lives (fleet_volumes carries no owner column). Identity only — used to make a
// divergence actionable. repository.FleetAppRepository satisfies it.
type LedgerAppReader interface {
	FindByID(ctx context.Context, id uuid.UUID) (*domain.FleetApp, error)
}

// LedgerObjectReader is the object-store presence probe for recovery points.
// ArtifactStore satisfies it. Exists must distinguish "absent" (false, nil) from
// "cannot tell" (_, err) — the whole safety argument of the third direction
// rides on that distinction, and both the S3 and filesystem stores honour it.
type LedgerObjectReader interface {
	Exists(key string) (bool, error)
}

// volumeBackingSuffix is the extension the backing-file backend materializes a
// volume under (`<dir>/<volume-id>.ext4`, see infrastructure/volume.Ensure).
// It is the ONLY filename shape this reconciler will interpret as a volume.
const volumeBackingSuffix = ".ext4"

// LedgerDivergenceReport is one pass's tally. The three divergence counters are
// the numbers worth alerting on; Undetermined is the honest residue — entries
// this pass could not decide, which is NOT a statement that they are fine.
type LedgerDivergenceReport struct {
	VolumesChecked        int
	FilesChecked          int
	RecoveryPointsChecked int

	// RowsWithoutFile counts fleet_volumes rows on this host whose backing file
	// is absent. The dangerous direction: the data is gone, or the ledger lies.
	RowsWithoutFile int
	// FilesWithoutRow counts backing files in the volume directory with no
	// fleet_volumes row. Leaked bytes nobody can attribute.
	FilesWithoutRow int
	// RecoveryPointsWithoutObject counts recovery-point rows whose object is
	// absent from the object store — a backup that is counted but does not exist.
	RecoveryPointsWithoutObject int
	// Undetermined counts entries whose verdict could not be established (DB or
	// store error, an unrecognized filename, a volume mid-restore).
	Undetermined int
}

// Divergences is the total number of reported divergences across all classes.
func (r LedgerDivergenceReport) Divergences() int {
	return r.RowsWithoutFile + r.FilesWithoutRow + r.RecoveryPointsWithoutObject
}

// FleetLedgerReconciler audits this host's slice of the volume + recovery-point
// ledger against the host filesystem and the object store, and reports.
type FleetLedgerReconciler struct {
	volumes   LedgerVolumeReader
	resources LedgerResourceReader
	apps      LedgerAppReader
	store     LedgerObjectReader
	// volumeDir is the root the backing-file backend materializes under — the
	// same cfg.Fleet.VolumeDir handed to FleetVolumeManager.
	volumeDir string
	// selfHost scopes the whole pass to THIS host. Zero means the scope is
	// unknown and the pass does nothing: judging another host's files from here
	// would be wrong by construction (the file is not visible from this
	// filesystem, so its absence proves nothing at all).
	selfHost uuid.UUID
}

// NewFleetLedgerReconciler constructs the reconciler. apps and store may be nil:
// without apps a divergence is still reported, only without its owner org;
// without a store the recovery-point direction is skipped and said so, rather
// than reporting every recovery point as missing.
func NewFleetLedgerReconciler(
	volumes LedgerVolumeReader,
	resources LedgerResourceReader,
	apps LedgerAppReader,
	store LedgerObjectReader,
	volumeDir string,
) *FleetLedgerReconciler {
	return &FleetLedgerReconciler{
		volumes:   volumes,
		resources: resources,
		apps:      apps,
		store:     store,
		volumeDir: volumeDir,
	}
}

// SetHostScope wires this instance's fleet host identity, which scopes the pass
// to the volumes pinned here. Wired after self-registration (the host id does
// not exist before it); without it Reconcile is a no-op.
func (uc *FleetLedgerReconciler) SetHostScope(selfHost uuid.UUID) { uc.selfHost = selfHost }

// Reconcile runs one report-only pass over both directions and returns the
// tally. It mutates nothing.
//
// An error is returned only when a whole direction's work list could not be
// obtained (e.g. the ledger query failed) — in that case NO divergence is
// reported for it, because an unreadable oracle is the definition of "cannot
// determine". Per-entry failures are logged and counted as Undetermined.
func (uc *FleetLedgerReconciler) Reconcile(ctx context.Context) (LedgerDivergenceReport, error) {
	var rep LedgerDivergenceReport
	log := logger.FromContext(ctx)

	if uc.selfHost == uuid.Nil {
		log.Warn("fleet ledger reconcile: skipped, this instance has no fleet host identity to scope by")
		return rep, nil
	}
	if uc.volumes == nil {
		return rep, errors.New("ledger reconcile needs the volume ledger")
	}

	rows, err := uc.volumes.ListByHost(ctx, uc.selfHost)
	if err != nil {
		// The ledger IS the oracle for both the row→file direction and (via each
		// volume's app) the recovery-point direction. Without it nothing is
		// decidable, and a scan of the directory alone would report every file as
		// unattributable — i.e. an outage would print a page of false divergences.
		return rep, fmt.Errorf("list volumes of host %s: %w", uc.selfHost, err)
	}

	uc.reportRowsWithoutFile(ctx, rows, &rep)
	uc.reportFilesWithoutRow(ctx, &rep)
	uc.reportRecoveryPointsWithoutObject(ctx, rows, &rep)
	return rep, nil
}

// reportRowsWithoutFile is the dangerous direction: a ledger row on THIS host
// whose backing file is not here.
func (uc *FleetLedgerReconciler) reportRowsWithoutFile(ctx context.Context, rows []domain.Volume, rep *LedgerDivergenceReport) {
	log := logger.FromContext(ctx)
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		vol := &rows[i]
		if vol.BackingPath == "" {
			// The ledger makes no claim about a file yet (materialize has not run),
			// so there is nothing to diverge from.
			continue
		}
		if vol.Status == domain.VolumeStatusRestoring {
			// A restore swaps the live backing file by rename, so there is a window
			// in which the path legitimately does not exist. Absence here is not
			// evidence of anything.
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: volume is mid-restore, its backing file's presence proves nothing",
				"volume_id", vol.ID, "app_id", vol.AppID, "backing_path", vol.BackingPath)
			continue
		}
		rep.VolumesChecked++

		_, serr := os.Stat(vol.BackingPath)
		switch {
		case serr == nil:
			continue
		case os.IsNotExist(serr):
			rep.RowsWithoutFile++
			resID, ownerOrg := uc.identify(ctx, vol)
			log.Error("fleet ledger reconcile: DIVERGENCE row-without-file — the ledger advertises a volume whose backing file is absent on its affinity host; the data is gone or the ledger is wrong. Nothing was deleted or repaired.",
				"divergence", "row_without_file",
				"volume_id", vol.ID,
				"app_id", vol.AppID,
				"resource_id", resID,
				"owner_org", ownerOrg,
				"backing_path", vol.BackingPath,
				"expected_size_mb", vol.SizeMB,
				"volume_status", string(vol.Status),
				"mount_path", vol.MountPath,
				"attached_replica", vol.AttachedReplica,
				"host_id", uc.selfHost)
		default:
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: cannot determine whether a volume's backing file exists, reporting nothing about it",
				"volume_id", vol.ID, "backing_path", vol.BackingPath, "err", serr)
		}
	}
}

// reportFilesWithoutRow walks the volume directory and reports backing files no
// ledger row claims.
//
// Only `<uuid>.ext4` is interpreted — the exact shape the backend materializes.
// Anything else is either a sibling another sweep already owns (a restore's
// `.prerestore` / `.failed-<rp>` copy, a `.restore-<rp>.tmp` staging file) or a
// name this service did not mint, and a name that cannot be attributed is never
// guessed at.
//
// Known residue, deliberately not covered: the restore siblings of a volume
// whose ROW has since gone are leaked bytes too, but attributing them requires
// the same lookup on a name whose meaning is already ambiguous. They are named
// in the log only when the name is unrecognized entirely.
func (uc *FleetLedgerReconciler) reportFilesWithoutRow(ctx context.Context, rep *LedgerDivergenceReport) {
	log := logger.FromContext(ctx)
	if uc.volumeDir == "" {
		return
	}
	entries, err := os.ReadDir(uc.volumeDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No volume directory means no volumes were ever materialized here.
			return
		}
		rep.Undetermined++
		log.Warn("fleet ledger reconcile: cannot read the volume directory, reporting no unattributed files",
			"dir", uc.volumeDir, "err", err)
		return
	}

	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		id, ok := volumeIDFromBackingName(name)
		if !ok {
			if isKnownVolumeSibling(name) {
				continue
			}
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: unrecognized file in the volume directory, cannot attribute it to a volume",
				"dir", uc.volumeDir, "file", name)
			continue
		}
		rep.FilesChecked++

		path := filepath.Join(uc.volumeDir, name)
		_, ferr := uc.volumes.FindByID(ctx, id)
		switch {
		case ferr == nil:
			// A row exists. It may be pinned to another host (a stale copy of a
			// migrated volume), which is a different question this pass does not
			// answer — the bytes are attributable, so they are not leaked.
			continue
		case errors.Is(ferr, domain.ErrVolumeNotFound):
			rep.FilesWithoutRow++
			log.Error("fleet ledger reconcile: DIVERGENCE file-without-row — a volume backing file on this host has no fleet_volumes row; these bytes are unattributable and nothing will ever reclaim them. Nothing was deleted.",
				"divergence", "file_without_row",
				"volume_id", id,
				"path", path,
				"size_bytes", fileSizeOrZero(e),
				"host_id", uc.selfHost)
		default:
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: cannot determine whether a backing file has a ledger row, reporting nothing about it",
				"volume_id", id, "path", path, "err", ferr)
		}
	}
}

// reportRecoveryPointsWithoutObject reports recovery-point rows whose object is
// absent from the object store — the worst class, because a backup that does not
// exist is counted as protection.
//
// Scoping: the object store is SHARED, but a recovery point belongs to a
// resource, and a dedicated resource's volume is pinned to exactly one host. So
// walking this host's volumes → their live resource → its recovery points gives
// each row exactly one auditing host, with no cross-host judgement and no
// duplicate reports.
//
// Known residue, deliberately not covered: the recovery points of a TOMBSTONED
// resource (its app and volume are gone, so no host owns it) and those of a
// shared-tier resource (no volume at all) are not reachable from here. Auditing
// them needs a ledger-wide pass with a single elected owner, which does not
// exist yet.
func (uc *FleetLedgerReconciler) reportRecoveryPointsWithoutObject(ctx context.Context, rows []domain.Volume, rep *LedgerDivergenceReport) {
	log := logger.FromContext(ctx)
	if uc.resources == nil {
		return
	}
	if uc.store == nil {
		log.Warn("fleet ledger reconcile: no object store wired, recovery points NOT audited (their existence is unknown, not proven)")
		return
	}

	seen := make(map[uuid.UUID]struct{}, len(rows))
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		appID := rows[i].AppID
		if appID == uuid.Nil {
			continue
		}
		if _, dup := seen[appID]; dup {
			continue
		}
		seen[appID] = struct{}{}

		res, err := uc.resources.FindLiveResourceByApp(ctx, appID)
		if err != nil {
			if errors.Is(err, domain.ErrResourceNotFound) {
				// The ordinary case: an app with a volume but no durable resource
				// claim. Not a fault and not a divergence.
				continue
			}
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: cannot resolve an app's resource claim, its recovery points were not audited",
				"app_id", appID, "err", err)
			continue
		}
		points, perr := uc.resources.ListRecoveryPoints(ctx, res.ID)
		if perr != nil {
			rep.Undetermined++
			log.Warn("fleet ledger reconcile: cannot list a resource's recovery points, they were not audited",
				"resource_id", res.ID, "owner_org", res.OwnerOrg, "err", perr)
			continue
		}
		for j := range points {
			if ctx.Err() != nil {
				return
			}
			uc.auditRecoveryPoint(ctx, res, &points[j], rep)
		}
	}
}

// auditRecoveryPoint probes one recovery point's object.
func (uc *FleetLedgerReconciler) auditRecoveryPoint(
	ctx context.Context,
	res *domain.FleetResource,
	rp *domain.FleetResourceRecoveryPoint,
	rep *LedgerDivergenceReport,
) {
	log := logger.FromContext(ctx)
	rep.RecoveryPointsChecked++

	// An empty object key is the same failure with no probe needed: the row
	// counts as a backup and references nothing.
	if rp.ObjectKey == "" {
		rep.RecoveryPointsWithoutObject++
		log.Error("fleet ledger reconcile: DIVERGENCE recovery-point-without-object — a recovery point row carries no object key, so it protects nothing while being counted as a backup. Nothing was deleted.",
			"divergence", "recovery_point_without_object",
			"recovery_point_id", rp.ID,
			"resource_id", res.ID,
			"owner_org", res.OwnerOrg,
			"volume_id", rp.VolumeID,
			"object_key", "",
			"expected_size_bytes", rp.SizeBytes,
			"kind", rp.Kind,
			"created_at", rp.CreatedAt.UTC())
		return
	}

	present, err := uc.store.Exists(rp.ObjectKey)
	switch {
	case err != nil:
		rep.Undetermined++
		log.Warn("fleet ledger reconcile: cannot determine whether a recovery point's object exists, reporting nothing about it",
			"recovery_point_id", rp.ID, "resource_id", res.ID, "object_key", rp.ObjectKey, "err", err)
	case !present:
		rep.RecoveryPointsWithoutObject++
		log.Error("fleet ledger reconcile: DIVERGENCE recovery-point-without-object — the object store has no object for a recovery point the ledger counts as a backup; a restore from it would fail. Nothing was deleted.",
			"divergence", "recovery_point_without_object",
			"recovery_point_id", rp.ID,
			"resource_id", res.ID,
			"owner_org", res.OwnerOrg,
			"volume_id", rp.VolumeID,
			"object_key", rp.ObjectKey,
			"expected_size_bytes", rp.SizeBytes,
			"kind", rp.Kind,
			"created_at", rp.CreatedAt.UTC())
	}
}

// identify resolves the acting-on identity for a volume divergence: the owning
// org (from the app row) and the resource claim, when they can be resolved.
// Best-effort by design — it enriches a log line, so an unresolvable field is
// reported empty rather than suppressing the divergence itself.
func (uc *FleetLedgerReconciler) identify(ctx context.Context, vol *domain.Volume) (resourceID string, ownerOrg string) {
	if uc.apps != nil {
		if app, err := uc.apps.FindByID(ctx, vol.AppID); err == nil && app != nil {
			ownerOrg = app.OwnerOrg
		}
	}
	if uc.resources != nil {
		if res, err := uc.resources.FindLiveResourceByApp(ctx, vol.AppID); err == nil && res != nil {
			resourceID = res.ID.String()
			if ownerOrg == "" {
				ownerOrg = res.OwnerOrg.String()
			}
		}
	}
	return resourceID, ownerOrg
}

// volumeIDFromBackingName parses `<uuid>.ext4` — the exact name the backing-file
// backend materializes a volume under. Anything else is not interpreted.
func volumeIDFromBackingName(name string) (uuid.UUID, bool) {
	base, ok := strings.CutSuffix(name, volumeBackingSuffix)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(base)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// isKnownVolumeSibling reports whether a filename is one another sweep already
// owns: a restore's pre-restore original / rolled-back forensic copy (reclaimed
// by the backend when the volume is deleted) or a restore staging file
// (reclaimed by SweepInterruptedRestores). Naming them as unattributed leaks
// would be noise, not a finding.
func isKnownVolumeSibling(name string) bool {
	if strings.HasPrefix(name, ".restore-") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	base, _, found := strings.Cut(name, volumeBackingSuffix+".")
	if !found {
		return false
	}
	_, err := uuid.Parse(base)
	return err == nil
}

// fileSizeOrZero reports an entry's apparent size for the log line. Best-effort:
// a stat failure must not suppress the divergence it is describing.
func fileSizeOrZero(e os.DirEntry) int64 {
	info, err := e.Info()
	if err != nil {
		return 0
	}
	return info.Size()
}
