//go:build integration

// External test package (postgres_test), not the in-package `postgres` one: this
// file needs internal/usecase (the allocator + the reconcile), and usecase already
// imports this package — an in-package test would be an import cycle.
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"
)

const (
	itUIDBase = 100000
	itUIDSpan = 8192
)

// startLeasePG boots a throwaway Postgres and returns a gorm handle plus a
// golang-migrate instance over the SAME connection, so a test can migrate to an
// intermediate version (which is how the 0020 BACKFILL is exercised: seed the
// pre-0020 world, then apply it).
func startLeasePG(t *testing.T) (*gorm.DB, *migrate.Migrate) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("runtime"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	p, _ := strconv.Atoi(port.Port())

	var db *gorm.DB
	for i := 0; i < 30; i++ {
		db, err = postgres.NewDB(postgres.Config{
			Host: host, Port: p, User: "postgres", Password: "postgres",
			Database: "runtime", SSLMode: "disable",
		})
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrap sql.DB: %v", err)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open migration source: %v", err)
	}
	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	return db, m
}

// migrateAll applies every migration.
func migrateAll(t *testing.T, m *migrate.Migrate) {
	t.Helper()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
}

// seedHost inserts a fleet host row. The ordinal is left NULL — assignment is
// EnsureHostOrdinal's job, which is itself under test.
func seedHost(t *testing.T, db *gorm.DB, id uuid.UUID, created time.Time) {
	t.Helper()
	// failure_domain is NOT NULL with NO DEFAULT since migration 0022: every host
	// row must state which site/power/network it shares a fate with, and there is
	// deliberately nothing for a writer to fall back to. Tests that pin an EARLIER
	// schema version (the 0020 backfill continuity test) seed the same host without
	// the column, so the insert is chosen from the schema that is actually applied
	// rather than from the newest one.
	var hasFailureDomain int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'fleet_hosts' AND column_name = 'failure_domain'`).Scan(&hasFailureDomain).Error; err != nil {
		t.Fatalf("introspect fleet_hosts: %v", err)
	}

	stmt := `INSERT INTO fleet_hosts
		(id, region, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, 'homelab', '{}', 4, 8192, 40000, 4, 8192, 40000, 'healthy', 'active', '10.0.10.244:50061', ?, ?)`
	if hasFailureDomain > 0 {
		stmt = `INSERT INTO fleet_hosts
		(id, region, failure_domain, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, 'homelab', 'site-a/breaker-a/switch-1', '{}', 4, 8192, 40000, 4, 8192, 40000, 'healthy', 'active', '10.0.10.244:50061', ?, ?)`
	}
	err := db.Exec(stmt, id, created, created).Error
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
}

func seedApp(t *testing.T, db *gorm.DB, id uuid.UUID, componentID string) {
	t.Helper()
	err := db.Exec(`INSERT INTO fleet_apps (id, component_id, env, image_repository, image_digest, owner_org)
		VALUES (?, ?, 'prod', 'repo', 'sha256:abc', '11111111-1111-1111-1111-111111111111')`,
		id, componentID).Error
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
}

func seedReplica(t *testing.T, db *gorm.DB, id, appID, hostID uuid.UUID, state string, netIndex int, guestIP, tap string, pid *int, created time.Time) {
	t.Helper()
	err := db.Exec(`INSERT INTO fleet_replicas
		(id, app_id, host_id, state, guest_ip, net_index, tap_name, pid, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, appID, hostID, state, guestIP, netIndex, tap, pid, created, created).Error
	if err != nil {
		t.Fatalf("seed replica: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The fences
// ─────────────────────────────────────────────────────────────────────

// TestNetLeaseFencesRejectEveryDuplicate drives the FIVE unique indexes directly.
// Each one is a distinct way two microVMs could end up sharing something that must
// never be shared, and the test asserts on the DATABASE, not on the allocator —
// the allocator's correctness is worth nothing if the fence behind it is missing.
func TestNetLeaseFencesRejectEveryDuplicate(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()
	repo := postgres.NewNetLeaseRepository(db)

	hostA := uuid.New()
	hostB := uuid.New()
	seedHost(t, db, hostA, time.Now().Add(-time.Hour))
	seedHost(t, db, hostB, time.Now())

	base, err := domain.DeriveNetLease(0, 1, itUIDBase)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	held := base
	held.ID = uuid.New()
	held.HostID = hostA
	held.OwnerKind, held.OwnerID = domain.NetLeaseOwnerReplica, uuid.New()
	held.CreatedAt, held.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := repo.Acquire(ctx, &held); err != nil {
		t.Fatalf("acquire the first lease: %v", err)
	}

	// Each mutation below changes ONLY the fields needed to keep the OTHER fences
	// happy, so exactly one fence can be the reason for the rejection.
	tests := []struct {
		name   string
		mutate func(l *domain.NetLease)
	}{
		{
			name: "net_index is fleet-global: another host may not take the same /30",
			mutate: func(l *domain.NetLease) {
				l.HostID = hostB // different host ⇒ slot/uid/tap fences do not apply
				l.OwnerID = uuid.New()
			},
		},
		{
			name: "local_slot is the jail id: one host may not hand it out twice",
			mutate: func(l *domain.NetLease) {
				l.NetIndex = 5000
				l.HostIP, l.GuestIP = "10.201.78.65", "10.201.78.66"
				l.VMUID = itUIDBase + 900
				l.TapName = "img900"
				l.OwnerID = uuid.New()
				// local_slot stays 1 — the fence under test.
			},
		},
		{
			name: "vm_uid is the unprivileged identity: one host may not hand it out twice",
			mutate: func(l *domain.NetLease) {
				l.NetIndex = 5001
				l.HostIP, l.GuestIP = "10.201.78.65", "10.201.78.66"
				l.LocalSlot = 900
				l.TapName = "img900"
				l.OwnerID = uuid.New()
				// vm_uid stays base+1 — the fence under test.
			},
		},
		{
			name: "tap_name is a host device: one host may not create it twice",
			mutate: func(l *domain.NetLease) {
				l.NetIndex = 5002
				l.HostIP, l.GuestIP = "10.201.78.65", "10.201.78.66"
				l.LocalSlot = 901
				l.VMUID = itUIDBase + 901
				l.OwnerID = uuid.New()
				// tap_name stays img1 — the fence under test.
			},
		},
		{
			name: "(owner_kind, owner_id) is at-most-once: one row may not hold two leases",
			mutate: func(l *domain.NetLease) {
				l.NetIndex = 5003
				l.HostIP, l.GuestIP = "10.201.78.65", "10.201.78.66"
				l.LocalSlot = 902
				l.VMUID = itUIDBase + 902
				l.TapName = "img902"
				// owner stays the first lease's — the fence under test.
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := held
			candidate.ID = uuid.New()
			// The address columns carry no fence of their own (net_index is the /30's
			// identity), so the values above only need to be distinct and plausible.
			tt.mutate(&candidate)
			err := repo.Acquire(ctx, &candidate)
			if !errors.Is(err, domain.ErrNetLeaseConflict) {
				t.Fatalf("duplicate accepted (or wrong error): %v", err)
			}
		})
	}

	var count int64
	if err := db.Table("fleet_net_leases").Count(&count).Error; err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if count != 1 {
		t.Fatalf("lease rows = %d, want 1 — a fence let a duplicate through", count)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Concurrency
// ─────────────────────────────────────────────────────────────────────

// TestNetLeaseConcurrentAcquiresAgainstPostgres is the property the deleted
// in-memory allocator could not provide: 64 boots racing through TWO independent
// allocator instances (standing in for two processes / a restart mid-flight) get 64
// DISTINCT allocations, with no duplicate address, uid, tap or slot anywhere.
func TestNetLeaseConcurrentAcquiresAgainstPostgres(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	repo := postgres.NewNetLeaseRepository(db)
	ord, err := repo.EnsureHostOrdinal(ctx, hostID)
	if err != nil {
		t.Fatalf("EnsureHostOrdinal: %v", err)
	}

	a := usecase.NewFleetNetAllocator(repo, hostID, ord, itUIDBase, itUIDSpan)
	b := usecase.NewFleetNetAllocator(repo, hostID, ord, itUIDBase, itUIDSpan)

	const n = 64
	var wg sync.WaitGroup
	leases := make([]domain.NetLease, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alloc := a
			if i%2 == 1 {
				alloc = b
			}
			leases[i], errs[i] = alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, uuid.New())
		}(i)
	}
	wg.Wait()

	seenSlot := map[int]bool{}
	seenIndex := map[int]bool{}
	seenUID := map[int]bool{}
	seenTap := map[string]bool{}
	seenGuest := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Acquire #%d: %v", i, err)
		}
		l := leases[i]
		if seenSlot[l.LocalSlot] {
			t.Fatalf("slot %d handed out twice", l.LocalSlot)
		}
		if seenIndex[l.NetIndex] {
			t.Fatalf("net index %d handed out twice", l.NetIndex)
		}
		if seenUID[l.VMUID] {
			t.Fatalf("vm uid %d handed out twice — two VMs would share one identity", l.VMUID)
		}
		if seenTap[l.TapName] {
			t.Fatalf("tap %s handed out twice", l.TapName)
		}
		if seenGuest[l.GuestIP] {
			t.Fatalf("guest ip %s handed out twice", l.GuestIP)
		}
		seenSlot[l.LocalSlot], seenIndex[l.NetIndex] = true, true
		seenUID[l.VMUID], seenTap[l.TapName], seenGuest[l.GuestIP] = true, true, true
	}

	var count int64
	if err := db.Table("fleet_net_leases").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Fatalf("lease rows = %d, want %d", count, n)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Host ordinals
// ─────────────────────────────────────────────────────────────────────

func TestEnsureHostOrdinalIsIdempotentUniqueAndBounded(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()
	repo := postgres.NewNetLeaseRepository(db)

	first, second := uuid.New(), uuid.New()
	seedHost(t, db, first, time.Now().Add(-time.Hour))
	seedHost(t, db, second, time.Now())

	got1, err := repo.EnsureHostOrdinal(ctx, first)
	if err != nil || got1 != 0 {
		t.Fatalf("first ordinal = %d, %v; want 0", got1, err)
	}
	got2, err := repo.EnsureHostOrdinal(ctx, second)
	if err != nil || got2 != 1 {
		t.Fatalf("second ordinal = %d, %v; want 1", got2, err)
	}
	// Idempotent: a re-register must NOT move a host's block (its live VMs are
	// addressed out of it).
	again, err := repo.EnsureHostOrdinal(ctx, first)
	if err != nil || again != 0 {
		t.Fatalf("re-ensure = %d, %v; want a stable 0", again, err)
	}

	// Fill the space, then prove the 17th host is REFUSED rather than aliased onto
	// an existing block.
	for i := 2; i <= domain.NetMaxOrdinal; i++ {
		id := uuid.New()
		seedHost(t, db, id, time.Now())
		if ord, err := repo.EnsureHostOrdinal(ctx, id); err != nil || ord != i {
			t.Fatalf("ordinal for host #%d = %d, %v; want %d", i, ord, err, i)
		}
	}
	overflow := uuid.New()
	seedHost(t, db, overflow, time.Now())
	if _, err := repo.EnsureHostOrdinal(ctx, overflow); !errors.Is(err, domain.ErrNetOrdinalExhausted) {
		t.Fatalf("17th host ordinal = %v, want ErrNetOrdinalExhausted", err)
	}

	// And the DDL itself refuses a duplicate, independently of the code path.
	if err := db.Exec(`UPDATE fleet_hosts SET net_ordinal = 0 WHERE id = ?`, second).Error; err == nil {
		t.Fatal("the unique index on fleet_hosts.net_ordinal accepted a duplicate")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Boot-time reconcile against a real database and a real process
// ─────────────────────────────────────────────────────────────────────

// itBooter records teardowns; nothing real can be booted in a test.
type itBooter struct {
	teardowns []usecase.ImageDecommissionInput
}

func (b *itBooter) BootTest(context.Context, usecase.ImageBootInput) (usecase.ImageTestResult, error) {
	return usecase.ImageTestResult{}, errors.New("not used")
}
func (b *itBooter) BootResident(context.Context, usecase.ImageBootInput) (usecase.ImageResidentResult, error) {
	return usecase.ImageResidentResult{}, errors.New("not used")
}
func (b *itBooter) Decommission(_ context.Context, in usecase.ImageDecommissionInput) error {
	b.teardowns = append(b.teardowns, in)
	return nil
}

type itReclaimer struct{ reclaimed []domain.NetLease }

func (r *itReclaimer) ReclaimLeaseArtifacts(_ context.Context, lease domain.NetLease) error {
	r.reclaimed = append(r.reclaimed, lease)
	return nil
}

// TestReconcileAdoptsALiveVMAndNeverReallocatesItsSlot is the restart scenario, with
// a REAL live process standing in for the VMM: after the reconcile the lease is
// still held, and the next boot on this host gets a DIFFERENT slot.
//
// A test that only asserted "adopted == 1" would miss the failure that matters —
// the next allocation taking the running VM's address.
func TestReconcileAdoptsALiveVMAndNeverReallocatesItsSlot(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()
	repo := postgres.NewNetLeaseRepository(db)

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	ord, err := repo.EnsureHostOrdinal(ctx, hostID)
	if err != nil {
		t.Fatalf("EnsureHostOrdinal: %v", err)
	}

	// A real, live child process: its pid is what the reconcile probes.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in vmm: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	pid := cmd.Process.Pid

	alloc := usecase.NewFleetNetAllocator(repo, hostID, ord, itUIDBase, itUIDSpan)
	appID := uuid.New()
	seedApp(t, db, appID, "comp-live")
	replicaID := uuid.New()
	lease, err := alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, replicaID)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	seedReplica(t, db, replicaID, appID, hostID, "resident", lease.NetIndex, lease.GuestIP, lease.TapName, &pid, time.Now())

	booter, reclaimer := &itBooter{}, &itReclaimer{}
	rec := usecase.NewFleetNetLeaseReconciler(repo, postgres.NewHostRepository(db), postgres.NewReplicaRepository(db),
		postgres.NewImageWorkloadRepository(db), booter, reclaimer, hostID, itUIDBase)

	report, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Adopted != 1 || report.Reclaimed != 0 || report.TornDown != 0 {
		t.Fatalf("report = %+v, want exactly one adoption", report)
	}
	if len(booter.teardowns) != 0 {
		t.Fatalf("a LIVE vm was torn down: %+v", booter.teardowns)
	}

	next, err := alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, uuid.New())
	if err != nil {
		t.Fatalf("next Acquire: %v", err)
	}
	if next.LocalSlot == lease.LocalSlot || next.NetIndex == lease.NetIndex || next.VMUID == lease.VMUID {
		t.Fatalf("the next boot took the LIVE vm's allocation: %+v vs %+v", next, lease)
	}
}

// TestReconcileTearsDownADeadOwnerAndFreesItsSlot is the fail-open hole this whole
// change closes: RefreshHealth marks a replica `dead` while its VMM keeps running,
// and the old allocator's seed did not treat `dead` as occupying — so the next boot
// took its address, uid and chroot. Now the VM is stopped FIRST and only then is
// the slot released.
func TestReconcileTearsDownADeadOwnerAndFreesItsSlot(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()
	repo := postgres.NewNetLeaseRepository(db)

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	ord, err := repo.EnsureHostOrdinal(ctx, hostID)
	if err != nil {
		t.Fatalf("EnsureHostOrdinal: %v", err)
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in vmm: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	pid := cmd.Process.Pid

	alloc := usecase.NewFleetNetAllocator(repo, hostID, ord, itUIDBase, itUIDSpan)
	appID := uuid.New()
	seedApp(t, db, appID, "comp-dead")
	replicaID := uuid.New()
	lease, err := alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, replicaID)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	seedReplica(t, db, replicaID, appID, hostID, "dead", lease.NetIndex, lease.GuestIP, lease.TapName, &pid, time.Now().Add(-time.Hour))

	booter, reclaimer := &itBooter{}, &itReclaimer{}
	rec := usecase.NewFleetNetLeaseReconciler(repo, postgres.NewHostRepository(db), postgres.NewReplicaRepository(db),
		postgres.NewImageWorkloadRepository(db), booter, reclaimer, hostID, itUIDBase)

	report, err := rec.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.TornDown != 1 || report.Reclaimed != 1 {
		t.Fatalf("report = %+v, want one teardown and one reclaim", report)
	}
	// The VM must have been stopped BEFORE the slot was freed, and through the
	// owner's recorded handle.
	if len(booter.teardowns) != 1 || booter.teardowns[0].PID != pid {
		t.Fatalf("teardowns = %+v, want one call carrying pid %d", booter.teardowns, pid)
	}
	if len(reclaimer.reclaimed) != 1 || reclaimer.reclaimed[0].TapName != lease.TapName {
		t.Fatalf("artifact reclaims = %+v, want the lease's tap %s", reclaimer.reclaimed, lease.TapName)
	}

	// The slot is genuinely free again — the same coordinates are re-issued.
	reused, err := alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, uuid.New())
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	if reused.LocalSlot != lease.LocalSlot {
		t.Fatalf("reclaimed slot not reused: got %d, want %d", reused.LocalSlot, lease.LocalSlot)
	}
}

// TestReconcileRefusesALeaselessOccupyingRow proves the collision signal survives a
// round-trip through Postgres, and that the refusal NAMES both rows: an operator
// has to be able to look at the host and decide which VM is real.
func TestReconcileRefusesALeaselessOccupyingRow(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	ctx := context.Background()
	repo := postgres.NewNetLeaseRepository(db)

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	ord, err := repo.EnsureHostOrdinal(ctx, hostID)
	if err != nil {
		t.Fatalf("EnsureHostOrdinal: %v", err)
	}
	alloc := usecase.NewFleetNetAllocator(repo, hostID, ord, itUIDBase, itUIDSpan)

	appID := uuid.New()
	seedApp(t, db, appID, "comp-collide")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in vmm: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	pid := cmd.Process.Pid

	// The winner holds the lease.
	winner := uuid.New()
	lease, err := alloc.Acquire(ctx, domain.NetLeaseOwnerReplica, winner)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	seedReplica(t, db, winner, appID, hostID, "resident", lease.NetIndex, lease.GuestIP, lease.TapName, &pid, time.Now())
	// The loser claims the same index with NO lease — what the 0020 backfill leaves
	// behind when two rows claimed one index.
	loser := uuid.New()
	seedReplica(t, db, loser, appID, hostID, "resident", lease.NetIndex, lease.GuestIP, lease.TapName, &pid, time.Now())

	booter, reclaimer := &itBooter{}, &itReclaimer{}
	rec := usecase.NewFleetNetLeaseReconciler(repo, postgres.NewHostRepository(db), postgres.NewReplicaRepository(db),
		postgres.NewImageWorkloadRepository(db), booter, reclaimer, hostID, itUIDBase)

	_, err = rec.Reconcile(ctx)
	if !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("Reconcile = %v, want ErrNetPlaneUnreconciled", err)
	}
	// The refusal must name BOTH sides of the collision: the row that holds the
	// address and the row that also claims it. Either one alone is not actionable.
	if !strings.Contains(err.Error(), loser.String()) {
		t.Fatalf("refusal does not name the leaseless row %s: %v", loser, err)
	}
	if !strings.Contains(err.Error(), winner.String()) {
		t.Fatalf("refusal does not name the row that HOLDS the address (%s): %v", winner, err)
	}
	// And nothing was destroyed on the way to refusing.
	if len(booter.teardowns) != 0 || len(reclaimer.reclaimed) != 0 {
		t.Fatalf("a fail-closed reconcile tore something down: %+v / %+v", booter.teardowns, reclaimer.reclaimed)
	}
	var count int64
	if err := db.Table("fleet_net_leases").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("lease rows = %d, want the winner's lease intact", count)
	}
}

// ─────────────────────────────────────────────────────────────────────
// The 0020 backfill
// ─────────────────────────────────────────────────────────────────────

// TestMigration0020BackfillReproducesTheLiveShape migrates to 0019, seeds the world
// as it exists on the live fleet host TODAY (one host; one resident replica at
// net_index 1; terminal workloads that hold nothing), then applies 0020 and asserts
// the resulting lease is EXACTLY the addressing that replica is running on.
//
// This is the live-continuity contract: if the backfill produced anything else, the
// boot-time reconcile would either refuse every boot on the real host or, worse,
// fence the wrong address.
func TestMigration0020BackfillReproducesTheLiveShape(t *testing.T) {
	db, m := startLeasePG(t)
	if err := m.Migrate(19); err != nil {
		t.Fatalf("migrate to 0019: %v", err)
	}

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now().Add(-24*time.Hour))
	appID := uuid.New()
	seedApp(t, db, appID, "comp-live")
	residentID := uuid.New()
	pid := 12345
	// The live row: net_index 1, guest 10.201.0.6, tap img1.
	seedReplica(t, db, residentID, appID, hostID, "resident", 1, "10.201.0.6", "img1", &pid, time.Now().Add(-time.Hour))
	// A terminal replica and terminal workloads hold NOTHING and must get no lease.
	seedReplica(t, db, uuid.New(), appID, hostID, "scheduled", 0, "", "", nil, time.Now())
	for i, state := range []string{"exited", "failed"} {
		err := db.Exec(`INSERT INTO image_workloads (id, class, state, net_index, guest_ip, tap_name, created_at, updated_at)
			VALUES (?, 'test', ?, ?, ?, ?, ?, ?)`,
			uuid.New(), state, 20+i, fmt.Sprintf("10.201.0.%d", 82+i*4), fmt.Sprintf("img%d", 20+i),
			time.Now(), time.Now()).Error
		if err != nil {
			t.Fatalf("seed terminal workload: %v", err)
		}
	}

	if err := m.Migrate(20); err != nil {
		t.Fatalf("migrate to 0020: %v", err)
	}

	// The host took ordinal 0 — the block its live VM is already addressed out of.
	var ordinal *int
	if err := db.Raw(`SELECT net_ordinal FROM fleet_hosts WHERE id = ?`, hostID).Scan(&ordinal).Error; err != nil {
		t.Fatalf("read ordinal: %v", err)
	}
	if ordinal == nil || *ordinal != 0 {
		t.Fatalf("backfilled ordinal = %v, want 0", ordinal)
	}

	var leases []domain.NetLease
	if err := db.Find(&leases).Error; err != nil {
		t.Fatalf("read leases: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("backfilled leases = %d, want exactly 1 (only the resident replica holds anything): %+v", len(leases), leases)
	}
	got := leases[0]
	want, err := domain.DeriveNetLease(0, 1, itUIDBase)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got.OwnerKind != domain.NetLeaseOwnerReplica || got.OwnerID != residentID {
		t.Errorf("lease owner = %s/%s, want replica/%s", got.OwnerKind, got.OwnerID, residentID)
	}
	if got.HostID != hostID || got.HostOrdinal != 0 || got.LocalSlot != 1 || got.NetIndex != 1 {
		t.Errorf("lease coordinates = {host:%s ord:%d slot:%d index:%d}, want {%s 0 1 1}",
			got.HostID, got.HostOrdinal, got.LocalSlot, got.NetIndex, hostID)
	}
	if got.HostIP != want.HostIP || got.GuestIP != want.GuestIP || got.TapName != want.TapName || got.VMUID != want.VMUID {
		t.Errorf("lease addressing = {%s %s %s %d}, want {%s %s %s %d} — the live VM's own addressing",
			got.HostIP, got.GuestIP, got.TapName, got.VMUID, want.HostIP, want.GuestIP, want.TapName, want.VMUID)
	}
	// And the whole point: the boot-time reconcile ADOPTS it rather than refusing.
	if got.GuestIP != "10.201.0.6" || got.TapName != "img1" || got.VMUID != 100001 {
		t.Errorf("live continuity broken: got %s/%s/uid %d, want 10.201.0.6/img1/100001", got.GuestIP, got.TapName, got.VMUID)
	}
}

// TestMigration0020IsReversible pins the up→down→up reversibility CI expects.
func TestMigration0020IsReversible(t *testing.T) {
	_, m := startLeasePG(t)
	migrateAll(t, m)
	if err := m.Migrate(19); err != nil {
		t.Fatalf("migrate down to 0019: %v", err)
	}
	if err := m.Migrate(20); err != nil {
		t.Fatalf("migrate back up to 0020: %v", err)
	}
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if dirty || version != 20 {
		t.Fatalf("version = %d (dirty=%v), want a clean 20", version, dirty)
	}
}
