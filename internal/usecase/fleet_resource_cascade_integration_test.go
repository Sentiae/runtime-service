//go:build integration

package usecase

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/testutil"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// cascadeVolumeBackend materializes a REAL on-disk file at the same path shape
// the production BackingStore uses (<dir>/<volume_id>.ext4), without mkfs.ext4 —
// that is a Linux-host tool and the fact under test here is the CASCADE and the
// file's SURVIVAL of it, not the filesystem inside the file.
type cascadeVolumeBackend struct{ t *testing.T }

func (b cascadeVolumeBackend) Ensure(_ context.Context, in VolumeEnsureInput) (VolumeEnsureOutput, error) {
	b.t.Helper()
	if err := os.MkdirAll(in.Dir, 0o750); err != nil {
		return VolumeEnsureOutput{}, err
	}
	path := filepath.Join(in.Dir, in.VolumeID.String()+".ext4")
	f, err := os.Create(path)
	if err != nil {
		return VolumeEnsureOutput{}, err
	}
	if terr := f.Truncate(1 << 20); terr != nil {
		_ = f.Close()
		return VolumeEnsureOutput{}, terr
	}
	return VolumeEnsureOutput{BackingPath: path}, f.Close()
}

func (b cascadeVolumeBackend) Delete(_ context.Context, backingPath string) error {
	return os.Remove(backingPath)
}

// TestFleetAppCascadeStrandsAResourceThatMustStillRetire pins, against real
// Postgres and the real migrations, the schema fact that is the ROOT of this
// whole failure class:
//
//   - fleet_volumes.app_id REFERENCES fleet_apps(id) ON DELETE CASCADE
//     (migrations/0001_fleet_control_plane.up.sql :89), so deleting an app row
//     silently deletes its volume ROWS.
//   - the on-host <dir>/<volume_id>.ext4 backing FILE is not in the database and
//     therefore SURVIVES that cascade.
//   - fleet_resources.app_id carries NO foreign key at all
//     (migrations/0012_create_fleet_resources.up.sql), so the resource row keeps
//     pointing at an app that no longer exists.
//
// Together those three leave a resource that can neither boot (a re-provision
// would mint a new app with a new EMPTY volume, which recoverExisting refuses)
// nor, before this fix, be decommissioned. The teardown is driven here through
// the REAL *FleetProvision so the already-gone verdict is a real
// ErrWorkloadNotFound off two real repository lookups, not a fake's answer.
func TestFleetAppCascadeStrandsAResourceThatMustStillRetire(t *testing.T) {
	ctx := context.Background()
	db := testutil.NewTestDB(t, "")
	if _, _, err := postgres.RunMigrations(db); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	apps := postgres.NewFleetAppRepository(db)
	replicas := postgres.NewReplicaRepository(db)
	volumes := postgres.NewVolumeRepository(db)
	resources := postgres.NewFleetResourceRepository(db)
	workloads := postgres.NewImageWorkloadRepository(db)

	// ── a dedicated resource with a backing app and a materialized volume ──
	org := uuid.New()
	now := time.Now().UTC()
	app := &domain.FleetApp{
		ID:              uuid.New(),
		ComponentID:     "resource/" + org.String() + "/orders-db",
		Env:             "prod",
		OwnerOrg:        org.String(),
		ImageRepository: "sentiae/pg",
		ImageDigest:     "sha256:abc",
		DesiredReplicas: 1,
		MinReplicas:     1,
		MaxReplicas:     1,
		Port:            residentPGPort,
		ResourcesVCPU:   2,
		ResourcesMemMB:  1024,
		RestartPolicy:   domain.RestartPolicyAlways,
		SecretRefs:      []string{"secret/data/pg#password"}, // fleet_apps.secret_refs is NOT NULL
		LastActiveAt:    now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := apps.Create(ctx, app); err != nil {
		t.Fatalf("create fleet app: %v", err)
	}

	volDir := t.TempDir()
	volMgr := NewFleetVolumeManager(volumes, cascadeVolumeBackend{t: t}, volDir)
	vols, err := volMgr.EnsureAppVolumes(ctx, app.ID, []VolumeSpecInput{{SizeMB: 64, MountPath: "/data"}})
	if err != nil {
		t.Fatalf("ensure app volumes: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("volumes = %d, want 1", len(vols))
	}
	volumeID := vols[0].ID
	backingPath := vols[0].BackingPath
	if backingPath == "" {
		t.Fatal("volume has no backing path")
	}
	if _, serr := os.Stat(backingPath); serr != nil {
		t.Fatalf("backing file not materialized: %v", serr)
	}

	res := &domain.FleetResource{
		ID:         uuid.New(),
		OwnerOrg:   org,
		ClaimKey:   "orders-db",
		Env:        "prod",
		Revision:   1,
		Class:      resourceClassPostgres,
		Tier:       resourceTierDedicated,
		Phase:      domain.FleetResourcePhaseReady,
		AppID:      &app.ID,
		SecretRefs: []string{"secret/data/pg#password"},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := resources.SaveResource(ctx, res); err != nil {
		t.Fatalf("save resource: %v", err)
	}
	// The recovery point the teardown will legitimately rely on. It exists BEFORE
	// the app vanishes — exactly like the real crash window, where the snapshot
	// succeeded and only the tombstone write was lost.
	priorPoint := &domain.FleetResourceRecoveryPoint{
		ID:         uuid.New(),
		ResourceID: res.ID,
		VolumeID:   &volumeID,
		ObjectKey:  "volumes/" + volumeID.String() + "/prior.ext4",
		Kind:       "snapshot",
		SizeBytes:  1 << 20,
		CreatedAt:  now,
	}
	if err := resources.SaveRecoveryPoint(ctx, priorPoint); err != nil {
		t.Fatalf("save recovery point: %v", err)
	}

	// ── the cascade ───────────────────────────────────────────────────────
	if err := db.Exec(`DELETE FROM fleet_apps WHERE id = ?`, app.ID).Error; err != nil {
		t.Fatalf("delete fleet app: %v", err)
	}

	var volRows int64
	if err := db.Raw(`SELECT count(*) FROM fleet_volumes WHERE app_id = ?`, app.ID).Scan(&volRows).Error; err != nil {
		t.Fatalf("count fleet_volumes: %v", err)
	}
	if volRows != 0 {
		t.Fatalf("fleet_volumes rows = %d, want 0 — the ON DELETE CASCADE this whole class rests on is gone", volRows)
	}
	if _, serr := os.Stat(backingPath); serr != nil {
		t.Fatalf("the backing FILE must survive the row cascade (that asymmetry is the bug's root): %v", serr)
	}
	// And the resource row still points at the vanished app: fleet_resources.app_id
	// has no FK, which is what makes recovery possible at all.
	stranded, err := resources.GetResourceByHandle(ctx, res.ID)
	if err != nil {
		t.Fatalf("reload resource: %v", err)
	}
	if stranded.AppID == nil || *stranded.AppID != app.ID {
		t.Fatalf("resource app_id = %v, want the vanished %v (no FK must have nulled or blocked it)", stranded.AppID, app.ID)
	}

	// ── the teardown must still retire it ─────────────────────────────────
	// Real *FleetProvision with the real orchestrator in front of it: the
	// already-gone verdict comes off fleet_apps (unknown → (false, nil)) falling
	// through to image_workloads (unknown → ErrWorkloadNotFound), which is the
	// exact production route. Neither the scheduler nor the replica runtime is
	// reachable on a missing-app decommission, so both are left nil.
	orch := NewFleetOrchestrator(apps, replicas, nil, nil)
	orch.SetVolumeManager(volMgr)
	prov := NewFleetProvision(ctx, workloads, nil, nil, t.TempDir(), "127.0.0.1")
	prov.SetOrchestrator(orch)

	// The REAL snapshotter: with the volume rows cascaded away it walks zero
	// volumes and returns ([], nil) — a vacuous success that creates nothing, so
	// guest control and the artifact store are never reached.
	snapshotter := NewFleetVolumeSnapshotter(nil, nil, volumes, replicas, resources)

	uc := NewFleetResourceProvisioner(prov, resources, replicas, snapshotter, testEngine())

	final, err := uc.DecommissionDedicated(ctx, res.ID, true)
	if err != nil {
		t.Fatalf("a resource whose backing app vanished must still be retirable: %v", err)
	}
	if final == nil {
		t.Fatal("teardown must report the recovery point it relied on")
	}
	// It must be the PRE-EXISTING point — the vacuous snapshot created nothing, and
	// reporting a fresh point here would claim a backup that was never made.
	if final.ID != priorPoint.ID {
		t.Fatalf("reported recovery point = %s, want the pre-existing %s", final.ID, priorPoint.ID)
	}
	var pointRows int64
	if err := db.Raw(`SELECT count(*) FROM fleet_resource_recovery_points WHERE resource_id = ?`, res.ID).Scan(&pointRows).Error; err != nil {
		t.Fatalf("count recovery points: %v", err)
	}
	if pointRows != 1 {
		t.Fatalf("recovery points = %d, want the 1 pre-existing point (none minted, none dropped)", pointRows)
	}

	tomb, err := resources.GetResourceByHandle(ctx, res.ID)
	if err != nil {
		t.Fatalf("reload tombstone: %v", err)
	}
	if tomb.Phase != domain.FleetResourcePhaseDecommissioned || tomb.DecommissionedAt == nil {
		t.Fatalf("resource not tombstoned: phase=%q at=%v", tomb.Phase, tomb.DecommissionedAt)
	}
}
