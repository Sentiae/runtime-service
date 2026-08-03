//go:build integration

// External test package (postgres_test) to match the sibling fleet integration
// tests in this directory.
package postgres_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Migration 0022 (SentiaeDB standard-ha slice 0, D-196) proven against a real
// Postgres. Everything asserted here is a fence the DATABASE enforces, because a
// fence enforced only in code is a fence with a race in it.

const testFailureDomain = "site-a/breaker-a/switch-1"

func seedResource(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	stmt := `INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'provisioning', 1, ?, ?)`
	// D-202/0025: durability is NOT NULL with no default — a dedicated claim is
	// durable, and every writer must say so.
	if hasResourceDurability(t, db) {
		stmt = `INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, durability, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'provisioning', 1, 'durable', ?, ?)`
	}
	err := db.Exec(stmt, id, uuid.New(), "claim-"+uuid.NewString(), now, now).Error
	if err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	return id
}

// up → down → up (§24). The down is lossy by nature — it destroys human-supplied
// failure-domain knowledge no query can reconstruct — which is exactly why it must
// leave a schema the up can be re-applied to.
func TestMigration0022IsReversible(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	hasColumn := func(table, name string) bool {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_name = ? AND column_name = ?`, table, name).Scan(&n).Error; err != nil {
			t.Fatalf("introspect %s.%s: %v", table, name, err)
		}
		return n > 0
	}
	hasTable := func(name string) bool {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM information_schema.tables
			WHERE table_name = ?`, name).Scan(&n).Error; err != nil {
			t.Fatalf("introspect table %s: %v", name, err)
		}
		return n > 0
	}

	for _, c := range []struct{ table, column string }{
		{"fleet_hosts", "failure_domain"},
		{"fleet_resources", "availability_class"},
		{"fleet_resources", "sync_degrade_policy"},
	} {
		if !hasColumn(c.table, c.column) {
			t.Fatalf("after up: %s.%s must exist", c.table, c.column)
		}
	}
	for _, tbl := range []string{"fleet_resource_members", "fleet_resource_leases", "failover_events"} {
		if !hasTable(tbl) {
			t.Fatalf("after up: table %s must exist", tbl)
		}
	}

	// failure_domain has NO DEFAULT in the final state: a default would be the
	// fail-open the column exists to close.
	var def *string
	if err := db.Raw(`SELECT column_default FROM information_schema.columns
		WHERE table_name = 'fleet_hosts' AND column_name = 'failure_domain'`).Scan(&def).Error; err != nil {
		t.Fatalf("read column default: %v", err)
	}
	if def != nil {
		t.Fatalf("fleet_hosts.failure_domain must have NO default, got %q", *def)
	}

	// Down to 0021 by VERSION, not by a relative step: a step means "undo whatever
	// is newest", which stops testing 0022 the moment 0023 exists.
	if err := m.Migrate(21); err != nil {
		t.Fatalf("migrate down to 0021: %v", err)
	}
	for _, c := range []struct{ table, column string }{
		{"fleet_hosts", "failure_domain"},
		{"fleet_resources", "availability_class"},
		{"fleet_resources", "sync_degrade_policy"},
	} {
		if hasColumn(c.table, c.column) {
			t.Fatalf("after down: %s.%s must be gone", c.table, c.column)
		}
	}
	for _, tbl := range []string{"fleet_resource_members", "fleet_resource_leases", "failover_events"} {
		if hasTable(tbl) {
			t.Fatalf("after down: table %s must be gone", tbl)
		}
	}

	migrateAll(t, m)
	if !hasColumn("fleet_hosts", "failure_domain") || !hasTable("failover_events") {
		t.Fatal("after the second up: the schema must be back")
	}
}

// The existing-row claim the migration comment makes, tested rather than asserted:
// a host row that predates 0022 survives it, carrying the 'unattested' sentinel —
// which is deliberately not a parseable failure domain, so the host is never
// counted as a domain by the placement gate.
func TestMigration0022DoesNotStrandTheExistingHostRow(t *testing.T) {
	db, m := startLeasePG(t)

	// The world as it is on the live fleet host: schema at 0021, one host row.
	if err := m.Migrate(21); err != nil {
		t.Fatalf("migrate to 0021: %v", err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO fleet_hosts
		(id, region, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, 'homelab', '{}', 4, 7941, 17542, 4, 7941, 17542, 'healthy', 'active', '10.0.10.244:50061', ?, ?)`,
		id, now, now).Error
	if err != nil {
		t.Fatalf("seed pre-0022 host: %v", err)
	}

	migrateAll(t, m)

	var got string
	if err := db.Raw(`SELECT failure_domain FROM fleet_hosts WHERE id = ?`, id).Scan(&got).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "unattested" {
		t.Fatalf("pre-existing host failure_domain = %q, want the 'unattested' sentinel", got)
	}

	// And a NEW row must state one: the transient default was dropped, so there is
	// nothing for a writer to fall back into.
	err = db.Exec(`INSERT INTO fleet_hosts
		(id, region, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, 'homelab', '{}', 4, 8192, 40000, 4, 8192, 40000, 'healthy', 'active', 'x:1', ?, ?)`,
		uuid.New(), now, now).Error
	if err == nil {
		t.Fatal("a host row without a failure domain must be REFUSED by the database")
	}

	// '' is not a value either: NOT NULL alone would permit it, and an empty domain
	// compares unequal to every other one — the fail-open in a shorter costume.
	err = db.Exec(`INSERT INTO fleet_hosts
		(id, region, failure_domain, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, 'homelab', '', '{}', 4, 8192, 40000, 4, 8192, 40000, 'healthy', 'active', 'x:1', ?, ?)`,
		uuid.New(), now, now).Error
	if err == nil {
		t.Fatal("an EMPTY failure domain must be refused by the database")
	}

	// Same for the region half of the invariant: two empty regions would compare
	// equal and satisfy "same region" vacuously.
	err = db.Exec(`INSERT INTO fleet_hosts
		(id, region, failure_domain, labels, capacity_vcpu, capacity_mem_mb, capacity_disk_mb,
		 allocatable_vcpu, allocatable_mem_mb, allocatable_disk_mb, health, status, endpoint, created_at, updated_at)
		VALUES (?, '', ?, '{}', 4, 8192, 40000, 4, 8192, 40000, 'healthy', 'active', 'x:1', ?, ?)`,
		uuid.New(), testFailureDomain, now, now).Error
	if err == nil {
		t.Fatal("an EMPTY region must be refused by the database")
	}
}

// Exactly one primary per resource, enforced by the DATABASE and not by code.
// This is the split-brain fence at the membership layer: a code-level check would
// be a read-then-write with a window in it, and two promotions racing through that
// window is precisely the failure this tier exists to prevent.
func TestFleetResourceMembersAllowOnlyOnePrimary(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	resourceID := seedResource(t, db)

	insertMember := func(role, state string) error {
		return db.Exec(`INSERT INTO fleet_resource_members
			(id, resource_id, app_id, role, generation, state, host_id)
			VALUES (?, ?, ?, ?, 1, ?, ?)`,
			uuid.New(), resourceID, uuid.New(), role, state, hostID).Error
	}

	if err := insertMember("primary", "streaming"); err != nil {
		t.Fatalf("first primary: %v", err)
	}
	if err := insertMember("primary", "streaming"); err == nil {
		t.Fatal("a SECOND live primary for one resource must be refused by the unique index")
	}
	// A standby alongside it is the whole point of the tier.
	if err := insertMember("standby", "streaming"); err != nil {
		t.Fatalf("standby member: %v", err)
	}
	// A RETIRED primary is outside the partial index, so the successor can take
	// the role without a delete — which is what makes promotion a single statement
	// rather than a delete-then-insert with a gap in it.
	if err := db.Exec(`UPDATE fleet_resource_members SET role='primary', state='retired'
		WHERE resource_id = ? AND role = 'primary'`, resourceID).Error; err != nil {
		t.Fatalf("retire the primary: %v", err)
	}
	if err := insertMember("primary", "promoted"); err != nil {
		t.Fatalf("a successor primary must be insertable once the old one is retired: %v", err)
	}

	// A DIFFERENT resource has its own primary — the index is per resource.
	other := seedResource(t, db)
	if err := db.Exec(`INSERT INTO fleet_resource_members
		(id, resource_id, app_id, role, generation, state, host_id)
		VALUES (?, ?, ?, 'primary', 1, 'streaming', ?)`,
		uuid.New(), other, uuid.New(), hostID).Error; err != nil {
		t.Fatalf("another resource's primary: %v", err)
	}

	// Vocabulary fences.
	if err := insertMember("arbiter", "streaming"); err == nil {
		t.Fatal("an unknown member role must be refused")
	}
	if err := insertMember("standby", "confused"); err == nil {
		t.Fatal("an unknown member state must be refused")
	}

	// ON DELETE RESTRICT, never SET NULL: a member whose parent vanished is an
	// allocation nobody owns — a VM holding customer data with no claim above it.
	if err := db.Exec(`DELETE FROM fleet_resources WHERE id = ?`, resourceID).Error; err == nil {
		t.Fatal("deleting a resource that still has members must be REFUSED")
	}
	if err := db.Exec(`DELETE FROM fleet_hosts WHERE id = ?`, hostID).Error; err == nil {
		t.Fatal("deleting a host that still holds members must be REFUSED")
	}
}

// The lease is the only authority on who is primary, and its FKs restrict for the
// same reason as the members'.
func TestFleetResourceLeaseShape(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	hostID := uuid.New()
	seedHost(t, db, hostID, time.Now())
	resourceID := seedResource(t, db)

	// ⚠ Every time value comes from the control-plane DB's own now(): the clock
	// authority is this database, never a host clock (migration 0022).
	if err := db.Exec(`INSERT INTO fleet_resource_leases
		(resource_id, generation, holder_host_id, holder_member_id, expires_at, renewed_at)
		VALUES (?, 1, ?, ?, now() + interval '15 seconds', now())`,
		resourceID, hostID, uuid.New()).Error; err != nil {
		t.Fatalf("grant lease: %v", err)
	}
	// One lease per resource — the primary key is what makes "who holds it now" a
	// lookup rather than a query with an ordering in it.
	if err := db.Exec(`INSERT INTO fleet_resource_leases
		(resource_id, generation, holder_host_id, holder_member_id, expires_at, renewed_at)
		VALUES (?, 2, ?, ?, now() + interval '15 seconds', now())`,
		resourceID, hostID, uuid.New()).Error; err == nil {
		t.Fatal("a second lease row for one resource must be refused")
	}
	if err := db.Exec(`INSERT INTO fleet_resource_leases
		(resource_id, generation, holder_host_id, holder_member_id, expires_at, renewed_at)
		VALUES (?, 0, ?, ?, now(), now())`,
		seedResource(t, db), hostID, uuid.New()).Error; err == nil {
		t.Fatal("generation 0 is not a generation and must be refused")
	}
	if err := db.Exec(`DELETE FROM fleet_hosts WHERE id = ?`, hostID).Error; err == nil {
		t.Fatal("deleting a host that still holds a lease must be REFUSED")
	}
}

// The cause taxonomy and the witness set are what make a published RTO honest.
// Both are unbackfillable, so the DDL refuses anything outside the vocabulary
// rather than letting free text accumulate.
func TestFailoverEventsTaxonomy(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	resourceID := seedResource(t, db)

	insert := func(cause string, witnesses string, from, to int) error {
		return db.Exec(`INSERT INTO failover_events
			(id, resource_id, cause, witnesses, from_generation, to_generation, outcome,
			 detected_at, promoted_at, client_writable_at)
			VALUES (?, ?, ?, `+witnesses+`, ?, ?, 'succeeded', now(), now(), now())`,
			uuid.New(), resourceID, cause, from, to).Error
	}

	if err := insert("real", `ARRAY['w1_lease_expired','w2_standby_replication_lost']`, 1, 2); err != nil {
		t.Fatalf("a real failover with W1+W2: %v", err)
	}
	// A drill and a switchover are operator-triggered, so they carry no witness —
	// and they must stay distinguishable from the real population forever.
	if err := insert("drill", `ARRAY[]::text[]`, 1, 2); err != nil {
		t.Fatalf("a drill: %v", err)
	}
	if err := insert("switchover", `ARRAY[]::text[]`, 1, 2); err != nil {
		t.Fatalf("a switchover: %v", err)
	}

	if err := insert("planned", `ARRAY[]::text[]`, 1, 2); err == nil {
		t.Fatal("a cause outside the taxonomy must be refused — the population it would pollute can never be re-labelled")
	}
	if err := insert("real", `ARRAY['w4_hunch']`, 1, 2); err == nil {
		t.Fatal("a witness outside the vocabulary must be refused")
	}
	if err := insert("real", `ARRAY[]::text[]`, 1, 2); err == nil {
		t.Fatal("a REAL failover with no witness is unattributable and must be refused")
	}
	if err := insert("drill", `ARRAY[]::text[]`, 2, 2); err == nil {
		t.Fatal("a promotion that did not advance the generation fenced nothing and must be refused")
	}

	// An RTO interval that ends before it starts is a clock or wiring bug, and it
	// would drag a published average down silently.
	if err := db.Exec(`INSERT INTO failover_events
		(id, resource_id, cause, witnesses, from_generation, to_generation, outcome,
		 detected_at, client_writable_at)
		VALUES (?, ?, 'drill', ARRAY[]::text[], 1, 2, 'succeeded', now(), now() - interval '1 minute')`,
		uuid.New(), resourceID).Error; err == nil {
		t.Fatal("a negative RTO interval must be refused")
	}

	if err := db.Exec(`DELETE FROM fleet_resources WHERE id = ?`, resourceID).Error; err == nil {
		t.Fatal("deleting a resource that still has failover history must be REFUSED")
	}
}

// The availability axes are their own columns with their own closed vocabularies,
// and they default to the WEAKER promise — a resource that says nothing must never
// read as protected.
func TestResourceAvailabilityColumns(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	id := seedResource(t, db)

	var class, policy string
	if err := db.Raw(`SELECT availability_class, sync_degrade_policy FROM fleet_resources WHERE id = ?`, id).
		Row().Scan(&class, &policy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if class != "single" || policy != "fail_closed" {
		t.Fatalf("defaults = (%q, %q), want (single, fail_closed)", class, policy)
	}

	if err := db.Exec(`UPDATE fleet_resources SET availability_class = 'ha' WHERE id = ?`, id).Error; err != nil {
		t.Fatalf("set ha: %v", err)
	}
	if err := db.Exec(`UPDATE fleet_resources SET availability_class = 'highly-available' WHERE id = ?`, id).Error; err == nil {
		t.Fatal("an availability class outside (single, ha) must be refused")
	}
	if err := db.Exec(`UPDATE fleet_resources SET sync_degrade_policy = 'whatever' WHERE id = ?`, id).Error; err == nil {
		t.Fatal("a degrade policy outside (fail_closed, fail_open) must be refused")
	}
}
