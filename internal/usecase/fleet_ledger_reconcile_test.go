package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes. The reconciler's ports are READ-ONLY by construction, so these
// fakes have no mutating surface at all — "it cannot delete a row" is a
// compile-time property here, not something a test has to catch.
// ─────────────────────────────────────────────────────────────────────

// ledgerResFake serves the P19 resource ledger reads.
type ledgerResFake struct {
	byApp   map[uuid.UUID]*domain.FleetResource
	points  map[uuid.UUID][]domain.FleetResourceRecoveryPoint
	findErr error
	listErr error
}

func (f *ledgerResFake) FindLiveResourceByApp(_ context.Context, appID uuid.UUID) (*domain.FleetResource, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	res, ok := f.byApp[appID]
	if !ok {
		return nil, domain.ErrResourceNotFound
	}
	return res, nil
}

func (f *ledgerResFake) ListRecoveryPoints(_ context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.points[resourceID], nil
}

// ledgerAppFake resolves the owner org of a volume's app.
type ledgerAppFake struct{ apps map[uuid.UUID]*domain.FleetApp }

func (f *ledgerAppFake) FindByID(_ context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	app, ok := f.apps[id]
	if !ok {
		return nil, domain.ErrFleetAppNotFound
	}
	return app, nil
}

// ledgerStoreFake is the object-store presence probe. err stands in for a store
// outage, which must resolve to "cannot determine" and never to "missing".
type ledgerStoreFake struct {
	present map[string]bool
	err     error
}

func (f *ledgerStoreFake) Exists(key string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.present[key], nil
}

// errFindVolumeRepo makes the per-file row lookup fail, standing in for a DB
// outage while the host's volume list itself was already read.
type errFindVolumeRepo struct{ *volRepoFake }

func (errFindVolumeRepo) FindByID(context.Context, uuid.UUID) (*domain.Volume, error) {
	return nil, errors.New("connection refused")
}

// errListVolumeRepo fails the work-list query itself.
type errListVolumeRepo struct{ *volRepoFake }

func (errListVolumeRepo) ListByHost(context.Context, uuid.UUID) ([]domain.Volume, error) {
	return nil, errors.New("connection refused")
}

// ─────────────────────────────────────────────────────────────────────
// Fixtures.
// ─────────────────────────────────────────────────────────────────────

// ledgerWorld is one arranged host: a volume directory, a volume ledger and the
// resource/app/store oracles.
type ledgerWorld struct {
	dir       string
	host      uuid.UUID
	volumes   LedgerVolumeReader
	resources *ledgerResFake
	apps      *ledgerAppFake
	store     *ledgerStoreFake
	// volIDs are every id the ledger was seeded with, so the mutation check can
	// re-read all of them afterwards.
	volIDs []uuid.UUID
}

func newLedgerWorld(t *testing.T) *ledgerWorld {
	t.Helper()
	return &ledgerWorld{
		dir:       t.TempDir(),
		host:      uuid.New(),
		volumes:   newVolRepoFake(),
		resources: &ledgerResFake{byApp: map[uuid.UUID]*domain.FleetResource{}, points: map[uuid.UUID][]domain.FleetResourceRecoveryPoint{}},
		apps:      &ledgerAppFake{apps: map[uuid.UUID]*domain.FleetApp{}},
		store:     &ledgerStoreFake{present: map[string]bool{}},
	}
}

// addVolume seeds a fleet_volumes row pinned to this host. withFile materializes
// the backing file too; without it the row is the live "20 GB advertised, zero
// bytes behind it" divergence.
func (w *ledgerWorld) addVolume(t *testing.T, status domain.VolumeStatus, withFile bool) *domain.Volume {
	t.Helper()
	appID := uuid.New()
	id := uuid.New()
	host := w.host
	vol := &domain.Volume{
		ID:           id,
		AppID:        appID,
		SizeMB:       20480,
		HostAffinity: &host,
		MountPath:    "/data",
		BackingPath:  filepath.Join(w.dir, id.String()+volumeBackingSuffix),
		Status:       status,
		DeviceName:   "/dev/vdb",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	repo, ok := w.volumes.(*volRepoFake)
	if !ok {
		t.Fatalf("addVolume needs the plain ledger fake")
	}
	if err := repo.Create(context.Background(), vol); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	w.volIDs = append(w.volIDs, id)
	w.apps.apps[appID] = &domain.FleetApp{ID: appID, OwnerOrg: uuid.New().String()}
	if withFile {
		writeLedgerFile(t, vol.BackingPath)
	}
	return vol
}

// addRecoveryPoint attaches a recovery point to the resource claim backing a
// volume's app, creating the claim if needed. present decides whether the object
// store has the object.
func (w *ledgerWorld) addRecoveryPoint(t *testing.T, vol *domain.Volume, objectKey string, present bool) {
	t.Helper()
	res, ok := w.resources.byApp[vol.AppID]
	if !ok {
		res = &domain.FleetResource{
			ID:       uuid.New(),
			OwnerOrg: uuid.New(),
			AppID:    &vol.AppID,
			Phase:    domain.FleetResourcePhaseReady,
		}
		w.resources.byApp[vol.AppID] = res
	}
	volID := vol.ID
	w.resources.points[res.ID] = append(w.resources.points[res.ID], domain.FleetResourceRecoveryPoint{
		ID:         uuid.New(),
		ResourceID: res.ID,
		VolumeID:   &volID,
		ObjectKey:  objectKey,
		Kind:       "volume_snapshot",
		SizeBytes:  4096,
		CreatedAt:  time.Now().UTC(),
	})
	if objectKey != "" && present {
		w.store.present[objectKey] = true
	}
}

func (w *ledgerWorld) reconciler() *FleetLedgerReconciler {
	rec := NewFleetLedgerReconciler(w.volumes, w.resources, w.apps, w.store, w.dir)
	rec.SetHostScope(w.host)
	return rec
}

func writeLedgerFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("ext4-ish bytes"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fsFingerprint hashes the volume directory's exact contents: every name, mode,
// size and byte. Comparing it before and after a pass is how the report-only
// guarantee is PROVEN rather than assumed.
func fsFingerprint(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			t.Fatalf("stat %s: %v", e.Name(), ierr)
		}
		body := ""
		if !e.IsDir() {
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("read %s: %v", e.Name(), rerr)
			}
			sum := sha256.Sum256(b)
			body = hex.EncodeToString(sum[:])
		}
		lines = append(lines, fmt.Sprintf("%s|dir=%t|mode=%s|size=%d|sha=%s", e.Name(), e.IsDir(), info.Mode(), info.Size(), body))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(fmt.Sprint(lines)))
	return hex.EncodeToString(sum[:])
}

// ledgerFingerprint renders every seeded volume row in full, so any field the
// pass touched shows up as a diff.
func ledgerFingerprint(t *testing.T, repo LedgerVolumeReader, ids []uuid.UUID) string {
	t.Helper()
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		vol, err := repo.FindByID(context.Background(), id)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s|err=%v", id, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("%+v", *vol))
	}
	sort.Strings(lines)
	return fmt.Sprint(lines)
}

// ─────────────────────────────────────────────────────────────────────
// The test.
// ─────────────────────────────────────────────────────────────────────

// TestLedgerReconcile is both the detection proof and the report-only proof:
// every case asserts the exact tally AND that the filesystem and the ledger rows
// are byte-identical afterwards. A reconciler that "fixed" a divergence would be
// unrecoverable in production, so not mutating is the load-bearing property.
func TestLedgerReconcile(t *testing.T) {
	tests := []struct {
		name string
		// arrange returns the reconciler under test plus the world it reads, so
		// the mutation check can fingerprint both.
		arrange func(t *testing.T, w *ledgerWorld)
		// mutate lets a case swap in a failing ledger AFTER seeding.
		want    LedgerDivergenceReport
		wantErr bool
		noScope bool
	}{
		{
			// The live #fleet-volume-row-without-backing-file case: a row
			// advertising 20 GB of customer data whose file is not on the host.
			name: "row with no file is reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				w.addVolume(t, domain.VolumeStatusAvailable, false)
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, RowsWithoutFile: 1},
		},
		{
			name: "row whose file is present is not reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				w.addVolume(t, domain.VolumeStatusAttached, true)
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1},
		},
		{
			// A restore swaps the live file by rename, so absence is expected mid-way
			// and proves nothing.
			name: "volume mid-restore is undetermined, never a divergence",
			arrange: func(t *testing.T, w *ledgerWorld) {
				w.addVolume(t, domain.VolumeStatusRestoring, false)
			},
			want: LedgerDivergenceReport{Undetermined: 1},
		},
		{
			name: "row that never claimed a file is not a divergence",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAvailable, false)
				vol.BackingPath = ""
				if err := w.volumes.(*volRepoFake).Update(context.Background(), vol); err != nil {
					t.Fatalf("update: %v", err)
				}
			},
			want: LedgerDivergenceReport{},
		},
		{
			name: "file with no row is reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				writeLedgerFile(t, filepath.Join(w.dir, uuid.NewString()+volumeBackingSuffix))
			},
			want: LedgerDivergenceReport{FilesChecked: 1, FilesWithoutRow: 1},
		},
		{
			// Restore siblings belong to other sweeps; naming them as leaks would be
			// noise, and guessing at them would be worse.
			name: "restore siblings and staging files are not leaks",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAvailable, true)
				writeLedgerFile(t, vol.BackingPath+".prerestore")
				writeLedgerFile(t, vol.BackingPath+".failed-"+uuid.NewString())
				writeLedgerFile(t, filepath.Join(w.dir, ".restore-"+uuid.NewString()+".tmp"))
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1},
		},
		{
			name: "unrecognized file is undetermined, never a divergence",
			arrange: func(t *testing.T, w *ledgerWorld) {
				writeLedgerFile(t, filepath.Join(w.dir, "warm-template.img"))
			},
			want: LedgerDivergenceReport{Undetermined: 1},
		},
		{
			// A DB outage must never be read as "this file is orphaned".
			name: "ledger lookup error yields cannot-determine, not a leak",
			arrange: func(t *testing.T, w *ledgerWorld) {
				writeLedgerFile(t, filepath.Join(w.dir, uuid.NewString()+volumeBackingSuffix))
				w.volumes = errFindVolumeRepo{w.volumes.(*volRepoFake)}
			},
			want: LedgerDivergenceReport{FilesChecked: 1, Undetermined: 1},
		},
		{
			// The whole work list is unreadable: the pass reports nothing at all,
			// rather than calling every file on the host unattributable.
			name: "ledger outage reports no divergence at all",
			arrange: func(t *testing.T, w *ledgerWorld) {
				writeLedgerFile(t, filepath.Join(w.dir, uuid.NewString()+volumeBackingSuffix))
				w.volumes = errListVolumeRepo{w.volumes.(*volRepoFake)}
			},
			want:    LedgerDivergenceReport{},
			wantErr: true,
		},
		{
			name: "recovery point with no object is reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAttached, true)
				w.addRecoveryPoint(t, vol, "volumes/"+vol.ID.String()+"/rp1.ext4.gz", false)
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1, RecoveryPointsChecked: 1, RecoveryPointsWithoutObject: 1},
		},
		{
			name: "recovery point whose object exists is not reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAttached, true)
				w.addRecoveryPoint(t, vol, "volumes/"+vol.ID.String()+"/rp1.ext4.gz", true)
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1, RecoveryPointsChecked: 1},
		},
		{
			name: "recovery point with no object key is reported",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAttached, true)
				w.addRecoveryPoint(t, vol, "", false)
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1, RecoveryPointsChecked: 1, RecoveryPointsWithoutObject: 1},
		},
		{
			// An object-store outage must never be read as "this backup is gone".
			name: "object store error yields cannot-determine, not a missing backup",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAttached, true)
				w.addRecoveryPoint(t, vol, "volumes/"+vol.ID.String()+"/rp1.ext4.gz", true)
				w.store.err = errors.New("dial tcp minio:9000: connect: connection refused")
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1, RecoveryPointsChecked: 1, Undetermined: 1},
		},
		{
			name: "recovery-point list error yields cannot-determine",
			arrange: func(t *testing.T, w *ledgerWorld) {
				vol := w.addVolume(t, domain.VolumeStatusAttached, true)
				w.addRecoveryPoint(t, vol, "volumes/"+vol.ID.String()+"/rp1.ext4.gz", true)
				w.resources.listErr = errors.New("connection refused")
			},
			want: LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1, Undetermined: 1},
		},
		{
			// Without a host identity the pass may judge nothing: a file that is not
			// on this filesystem proves nothing by being absent from it.
			name: "no host scope reports nothing",
			arrange: func(t *testing.T, w *ledgerWorld) {
				w.addVolume(t, domain.VolumeStatusAvailable, false)
				writeLedgerFile(t, filepath.Join(w.dir, uuid.NewString()+volumeBackingSuffix))
			},
			want:    LedgerDivergenceReport{},
			noScope: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newLedgerWorld(t)
			tt.arrange(t, w)

			rec := NewFleetLedgerReconciler(w.volumes, w.resources, w.apps, w.store, w.dir)
			if !tt.noScope {
				rec.SetHostScope(w.host)
			}

			fsBefore := fsFingerprint(t, w.dir)
			rowsBefore := ledgerFingerprint(t, w.volumes, w.volIDs)

			got, err := rec.Reconcile(context.Background())
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error when the ledger is unreadable, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got != tt.want {
				t.Errorf("report mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}

			// ⚠ The point of this test. A reconciler that repaired or reclaimed
			// anything here would be destroying customer data on the strength of a
			// possibly-unavailable oracle.
			if after := fsFingerprint(t, w.dir); after != fsBefore {
				t.Errorf("the volume directory was MUTATED by a report-only pass (before=%s after=%s)", fsBefore, after)
			}
			if after := ledgerFingerprint(t, w.volumes, w.volIDs); after != rowsBefore {
				t.Errorf("the ledger rows were MUTATED by a report-only pass\nbefore: %s\nafter:  %s", rowsBefore, after)
			}
		})
	}
}

// TestLedgerReconcile_NoStoreSkipsRecoveryPoints proves the third direction is
// SKIPPED rather than reported when there is no object store to ask: calling
// every backup missing because the store is unwired would be the exact false
// alarm this reconciler exists to avoid producing.
func TestLedgerReconcile_NoStoreSkipsRecoveryPoints(t *testing.T) {
	w := newLedgerWorld(t)
	vol := w.addVolume(t, domain.VolumeStatusAttached, true)
	w.addRecoveryPoint(t, vol, "volumes/"+vol.ID.String()+"/rp1.ext4.gz", false)

	rec := NewFleetLedgerReconciler(w.volumes, w.resources, w.apps, nil, w.dir)
	rec.SetHostScope(w.host)

	got, err := rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := LedgerDivergenceReport{VolumesChecked: 1, FilesChecked: 1}
	if got != want {
		t.Fatalf("report mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestLedgerReconcile_ScopedToThisHost proves the pass never judges another
// host's volume: its file is not visible from this filesystem, so its absence
// would otherwise be reported as data loss on every host in the fleet.
func TestLedgerReconcile_ScopedToThisHost(t *testing.T) {
	w := newLedgerWorld(t)
	other := uuid.New()
	foreign := &domain.Volume{
		ID:           uuid.New(),
		AppID:        uuid.New(),
		SizeMB:       20480,
		HostAffinity: &other,
		BackingPath:  "/var/lib/sentiae/volumes/elsewhere.ext4",
		Status:       domain.VolumeStatusAvailable,
	}
	if err := w.volumes.(*volRepoFake).Create(context.Background(), foreign); err != nil {
		t.Fatalf("seed foreign volume: %v", err)
	}
	w.volIDs = append(w.volIDs, foreign.ID)

	got, err := w.reconciler().Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got != (LedgerDivergenceReport{}) {
		t.Fatalf("another host's volume was judged from here: %+v", got)
	}
}
