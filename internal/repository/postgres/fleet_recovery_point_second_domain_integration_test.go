//go:build integration

// External test package (postgres_test) to match the sibling fleet integration
// tests in this directory.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"gorm.io/gorm"
)

// Migration 0023 (the second failure domain, D-192/D-195/D-199) proven against a
// real Postgres.
//
// ⚠ WHY THESE ARE DATABASE TESTS. The two-domain claim is a durability promise made
// to a customer, and the whole point of migration 0023 is that it is a RECORDED FACT
// rather than an inference. A fence enforced only in Go is a fence with a deploy
// between it and the data; these assert the ones the DATABASE holds.

// seedRecoveryPointResource inserts a resource a recovery point can hang off.
func seedRecoveryPointResource(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	stmt := `INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'ready', 1, ?, ?)`
	// D-202/0025: durability is NOT NULL with no default on the current schema.
	if hasResourceDurability(t, db) {
		stmt = `INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, durability, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'ready', 1, 'durable', ?, ?)`
	}
	err := db.Exec(stmt, id, uuid.New(), "claim-"+uuid.NewString(), now, now).Error
	if err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	return id
}

// up → down → up (§24). The down is lossy as KNOWLEDGE — it discards the only record
// of which copies exist off the chassis, and the second bucket is not listable so
// nothing can reconstruct it — which is exactly why it must leave a schema the up can
// be re-applied to.
func TestMigration0023IsReversible(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	hasColumn := func(name string) bool {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'fleet_resource_recovery_points' AND column_name = ?`, name).Scan(&n).Error; err != nil {
			t.Fatalf("introspect %s: %v", name, err)
		}
		return n > 0
	}
	cols := []string{"locations", "second_domain_store", "second_domain_at", "second_domain_error"}
	for _, c := range cols {
		if !hasColumn(c) {
			t.Fatalf("after up: fleet_resource_recovery_points.%s must exist", c)
		}
	}

	// Down to 0022 BY VERSION, not by a relative step: a step means "undo whatever is
	// newest", which stops testing 0023 the moment 0024 exists.
	if err := m.Migrate(22); err != nil {
		t.Fatalf("migrate down to 0022: %v", err)
	}
	for _, c := range cols {
		if hasColumn(c) {
			t.Fatalf("after down: fleet_resource_recovery_points.%s must be gone", c)
		}
	}

	migrateAll(t, m)
	for _, c := range cols {
		if !hasColumn(c) {
			t.Fatalf("after the second up: fleet_resource_recovery_points.%s must be back", c)
		}
	}
}

// TestMigration0023LeavesPreExistingRowsUnknown is the unbackfillable claim, tested
// rather than asserted. A recovery point written before this column can never be
// known to be one-domain or two — the bucket cannot be enumerated (D-199: LIST is
// 403) — so it must land as 'unknown' and be read as the WEAKEST class, never as
// "probably fine".
func TestMigration0023LeavesPreExistingRowsUnknown(t *testing.T) {
	db, m := startLeasePG(t)

	// The world as it is before this migration: schema at 0022, one recovery point.
	if err := m.Migrate(22); err != nil {
		t.Fatalf("migrate to 0022: %v", err)
	}
	resID := seedRecoveryPointResource(t, db)
	rpID := uuid.New()
	err := db.Exec(`INSERT INTO fleet_resource_recovery_points
		(id, resource_id, object_key, kind, size_bytes, created_at)
		VALUES (?, ?, 'volumes/v/legacy.ext4', 'snapshot', 4096, now())`, rpID, resID).Error
	if err != nil {
		t.Fatalf("seed pre-0023 recovery point: %v", err)
	}

	migrateAll(t, m)

	var got string
	if err := db.Raw(`SELECT locations FROM fleet_resource_recovery_points WHERE id = ?`, rpID).Scan(&got).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := string(domain.RecoveryPointLocationsUnknown); got != want {
		t.Fatalf("pre-existing recovery point locations = %q, want %q", got, want)
	}
	if domain.RecoveryPointLocations(got).InTwoFailureDomains() {
		t.Fatal("an 'unknown' row must never count as two failure domains")
	}
}

// TestRecoveryPointLocationsAreFencedByTheDatabase proves the vocabulary fence and —
// the load-bearing one — that the two-domain claim cannot be asserted without naming
// WHERE and WHEN. A row reading primary_and_second_domain with no store and no
// timestamp would be a durability claim with no evidence attached.
func TestRecoveryPointLocationsAreFencedByTheDatabase(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	resID := seedRecoveryPointResource(t, db)

	insert := func(locations, store string, at any) error {
		return db.Exec(`INSERT INTO fleet_resource_recovery_points
			(id, resource_id, object_key, kind, size_bytes, created_at, locations, second_domain_store, second_domain_at)
			VALUES (?, ?, ?, 'snapshot', 1, now(), ?, ?, ?)`,
			uuid.New(), resID, "volumes/v/"+uuid.NewString()+".ext4", locations, store, at).Error
	}

	if err := insert("teleported", "", nil); err == nil {
		t.Error("an unrecognized location class must be refused by the database")
	}
	if err := insert(string(domain.RecoveryPointLocationsSecondDomain), "", nil); err == nil {
		t.Error("the two-domain claim must be refused without a named store and a confirmation time")
	}
	if err := insert(string(domain.RecoveryPointLocationsSecondDomain), "r2:b", nil); err == nil {
		t.Error("the two-domain claim must be refused without a confirmation time")
	}
	if err := insert(string(domain.RecoveryPointLocationsSecondDomain), "", time.Now().UTC()); err == nil {
		t.Error("the two-domain claim must be refused without a named store")
	}
	// And the honest shapes are accepted.
	if err := insert(string(domain.RecoveryPointLocationsPrimaryOnly), "", nil); err != nil {
		t.Errorf("a primary_only row must be accepted: %v", err)
	}
	if err := insert(string(domain.RecoveryPointLocationsSecondDomain), "r2:b", time.Now().UTC()); err != nil {
		t.Errorf("a fully evidenced two-domain row must be accepted: %v", err)
	}
}

// TestRecoveryPointSecondDomainLedgerWrites drives the real repository against the
// real schema — which is also what proves the GORM tags MATCH the DDL. The fleet host
// runs AutoMigrate, so a tag that disagrees with migration 0023 is a schema change the
// migration never authored (the D-187 lesson); a Save through GORM followed by a
// read-back is what catches it.
func TestRecoveryPointSecondDomainLedgerWrites(t *testing.T) {
	ctx := context.Background()
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewFleetResourceRepository(db)
	resID := seedRecoveryPointResource(t, db)

	// A capture writes primary_only BEFORE the mirror is attempted, so a crash between
	// the two never leaves a row claiming a copy that does not exist.
	rp := &domain.FleetResourceRecoveryPoint{
		ID:          uuid.New(),
		ResourceID:  resID,
		ObjectKey:   "volumes/v/" + uuid.NewString() + ".ext4",
		Kind:        "snapshot",
		SizeBytes:   4096,
		Checksum:    "abc123",
		Consistency: domain.RecoveryPointGuestFrozen,
		Locations:   domain.RecoveryPointLocationsPrimaryOnly,
		CreatedAt:   time.Now().UTC(),
	}
	if err := repo.SaveRecoveryPoint(ctx, rp); err != nil {
		t.Fatalf("save recovery point: %v", err)
	}

	read := func() domain.FleetResourceRecoveryPoint {
		t.Helper()
		got, err := repo.GetRecoveryPointByRef(ctx, resID, rp.ObjectKey)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		return *got
	}
	if got := read(); got.Locations != domain.RecoveryPointLocationsPrimaryOnly {
		t.Fatalf("locations after capture = %q, want primary_only", got.Locations)
	}

	// A failed mirror records the cause and leaves the class alone.
	if err := repo.RecordRecoveryPointMirrorFailure(ctx, rp.ID, time.Now().UTC(), "dial r2: connection refused"); err != nil {
		t.Fatalf("record mirror failure: %v", err)
	}
	got := read()
	if got.Locations != domain.RecoveryPointLocationsPrimaryOnly {
		t.Errorf("a FAILED mirror changed locations to %q; it must stay primary_only", got.Locations)
	}
	if got.SecondDomainError == "" {
		t.Error("a failed mirror recorded no cause")
	}
	if got.SecondDomainAt != nil {
		t.Errorf("second_domain_at = %v after a FAILURE; that column is the two-domain claim's evidence", got.SecondDomainAt)
	}

	// A confirmed mirror promotes the row, names the store, stamps the time and clears
	// the failure.
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.MarkRecoveryPointInSecondDomain(ctx, rp.ID, "r2:sentiae-recovery-points", at); err != nil {
		t.Fatalf("mark in second domain: %v", err)
	}
	got = read()
	if !got.Locations.InTwoFailureDomains() {
		t.Errorf("locations after a confirmed mirror = %q", got.Locations)
	}
	if got.SecondDomainStore != "r2:sentiae-recovery-points" {
		t.Errorf("second_domain_store = %q", got.SecondDomainStore)
	}
	if got.SecondDomainAt == nil || !got.SecondDomainAt.UTC().Equal(at) {
		t.Errorf("second_domain_at = %v, want %v", got.SecondDomainAt, at)
	}
	if got.SecondDomainError != "" {
		t.Errorf("second_domain_error = %q after a confirmed copy, want empty", got.SecondDomainError)
	}

	// Idempotent: re-confirming must not restamp the time to a later one, which would
	// claim the copy is newer than it is.
	later := at.Add(time.Hour)
	if err := repo.MarkRecoveryPointInSecondDomain(ctx, rp.ID, "r2:sentiae-recovery-points", later); err != nil {
		t.Fatalf("re-mark in second domain: %v", err)
	}
	if got = read(); !got.SecondDomainAt.UTC().Equal(at) {
		t.Errorf("second_domain_at moved to %v on a re-confirmation, want it pinned at %v", got.SecondDomainAt, at)
	}

	// A missing row is a caller bug and says so, rather than passing silently.
	if err := repo.MarkRecoveryPointInSecondDomain(ctx, uuid.New(), "r2:b", at); err == nil {
		t.Error("marking an unknown recovery point must fail")
	}
	if err := repo.RecordRecoveryPointMirrorFailure(ctx, uuid.New(), at, "x"); err == nil {
		t.Error("recording a failure against an unknown recovery point must fail")
	}
	// An unnamed store is refused in Go, so the CHECK constraint is a fence and not
	// the error message an operator reads.
	if err := repo.MarkRecoveryPointInSecondDomain(ctx, rp.ID, "", at); err == nil {
		t.Error("the two-domain claim must be refused without a named store")
	}
}

// TestListRecoveryPointLocationsCensus proves the metric's read: counts per class,
// the oldest per class, and — the deliberate scope decision — that a DECOMMISSIONED
// resource's recovery points are still counted. They survive the tombstone and are
// still restorable customer data, so excluding them would under-report the blobs one
// machine's loss would destroy.
func TestListRecoveryPointLocationsCensus(t *testing.T) {
	ctx := context.Background()
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewFleetResourceRepository(db)

	live := seedRecoveryPointResource(t, db)
	dead := seedRecoveryPointResource(t, db)
	if err := db.Exec(`UPDATE fleet_resources SET decommissioned_at = now() WHERE id = ?`, dead).Error; err != nil {
		t.Fatalf("tombstone resource: %v", err)
	}

	now := time.Now().UTC()
	add := func(resID uuid.UUID, loc domain.RecoveryPointLocations, age time.Duration) {
		t.Helper()
		rp := &domain.FleetResourceRecoveryPoint{
			ID: uuid.New(), ResourceID: resID,
			ObjectKey: "volumes/v/" + uuid.NewString() + ".ext4",
			Kind:      "snapshot", SizeBytes: 1, Checksum: "c",
			Locations: loc, CreatedAt: now.Add(-age),
		}
		if loc.InTwoFailureDomains() {
			at := now.Add(-age)
			rp.SecondDomainStore = "r2:b"
			rp.SecondDomainAt = &at
		}
		if err := repo.SaveRecoveryPoint(ctx, rp); err != nil {
			t.Fatalf("save recovery point: %v", err)
		}
	}
	add(live, domain.RecoveryPointLocationsSecondDomain, time.Hour)
	add(live, domain.RecoveryPointLocationsPrimaryOnly, 5*time.Hour)
	add(live, domain.RecoveryPointLocationsPrimaryOnly, 30*time.Hour)
	add(dead, domain.RecoveryPointLocationsPrimaryOnly, 100*time.Hour)

	facts, err := repo.ListRecoveryPointLocations(ctx)
	if err != nil {
		t.Fatalf("list recovery point locations: %v", err)
	}
	byClass := map[string]int{}
	oldest := map[string]*time.Time{}
	for _, f := range facts {
		byClass[f.Locations] = f.Count
		oldest[f.Locations] = f.OldestCreatedAt
	}
	if got := byClass[string(domain.RecoveryPointLocationsSecondDomain)]; got != 1 {
		t.Errorf("two-domain count = %d, want 1", got)
	}
	// 3 and not 2: the tombstoned resource's copy counts.
	if got := byClass[string(domain.RecoveryPointLocationsPrimaryOnly)]; got != 3 {
		t.Errorf("primary_only count = %d, want 3 (a decommissioned resource's recovery points still exist in exactly one place)", got)
	}
	at := oldest[string(domain.RecoveryPointLocationsPrimaryOnly)]
	if at == nil {
		t.Fatal("primary_only class reported no oldest timestamp")
	}
	if age := now.Sub(*at); age < 99*time.Hour {
		t.Errorf("oldest primary_only age = %v, want ~100h (the oldest single-domain copy is what the alert keys on)", age)
	}
}

// TestListRecoveryPointsToMirrorBacklog proves the control-plane mirror's read
// (D-200) against a real Postgres: exactly the primary_only rows, OLDEST FIRST, and
// bounded by the limit.
//
// ⚠ The ordering is not cosmetic. The alert this backlog drains is an AGE
// (sentiae_fleet_recovery_point_oldest_single_domain_age_seconds), so a worker fed
// newest-first would look busy while the worst number never moved.
//
// ⚠ `unknown` must NOT appear. Migration 0023 is unbackfillable and the second bucket
// cannot be enumerated to reconstruct it (D-199: LIST is 403), so those rows are
// permanently unknown — and a query that swept them in would attempt a copy for every
// pre-0023 row on every pass, forever, most of them without a checksum to confirm it
// against.
func TestListRecoveryPointsToMirrorBacklog(t *testing.T) {
	ctx := context.Background()
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewFleetResourceRepository(db)

	resID := seedRecoveryPointResource(t, db)
	now := time.Now().UTC()
	add := func(key string, loc domain.RecoveryPointLocations, age time.Duration) {
		t.Helper()
		rp := &domain.FleetResourceRecoveryPoint{
			ID: uuid.New(), ResourceID: resID,
			ObjectKey: key, Kind: "snapshot", SizeBytes: 1, Checksum: "c",
			Locations: loc, CreatedAt: now.Add(-age),
		}
		if loc.InTwoFailureDomains() {
			at := now.Add(-age)
			rp.SecondDomainStore = "r2:b"
			rp.SecondDomainAt = &at
		}
		if err := repo.SaveRecoveryPoint(ctx, rp); err != nil {
			t.Fatalf("save recovery point: %v", err)
		}
	}
	add("volumes/v/oldest.ext4", domain.RecoveryPointLocationsPrimaryOnly, 72*time.Hour)
	add("volumes/v/middle.ext4", domain.RecoveryPointLocationsPrimaryOnly, 5*time.Hour)
	add("volumes/v/newest.ext4", domain.RecoveryPointLocationsPrimaryOnly, time.Hour)
	add("volumes/v/legacy.ext4", domain.RecoveryPointLocationsUnknown, 500*time.Hour)
	add("volumes/v/done.ext4", domain.RecoveryPointLocationsSecondDomain, 400*time.Hour)

	got, err := repo.ListRecoveryPointsToMirror(ctx, 10)
	if err != nil {
		t.Fatalf("list recovery points to mirror: %v", err)
	}
	want := []string{"volumes/v/oldest.ext4", "volumes/v/middle.ext4", "volumes/v/newest.ext4"}
	if len(got) != len(want) {
		t.Fatalf("backlog size = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ObjectKey != want[i] {
			t.Fatalf("backlog[%d] = %q, want %q (oldest first)", i, got[i].ObjectKey, want[i])
		}
	}

	// The limit bounds one pass, and it must keep taking from the OLD end.
	capped, err := repo.ListRecoveryPointsToMirror(ctx, 2)
	if err != nil {
		t.Fatalf("list capped: %v", err)
	}
	if len(capped) != 2 || capped[0].ObjectKey != want[0] || capped[1].ObjectKey != want[1] {
		t.Fatalf("capped backlog = %v, want the two oldest %v", capped, want[:2])
	}

	// A non-positive limit asks for nothing; it must not degrade into "everything".
	none, err := repo.ListRecoveryPointsToMirror(ctx, 0)
	if err != nil {
		t.Fatalf("list with limit 0: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("limit 0 returned %d rows", len(none))
	}
}
