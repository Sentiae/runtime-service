//go:build integration

// External test package (postgres_test) to match the sibling fleet integration
// tests in this directory.
package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// Migration 0025 (D-202 — protection attaches or the provision fails) proven
// against a real Postgres. Everything asserted here is a fence the DATABASE
// enforces, because a fence enforced only in code is a fence with a race in it.

// hasResourceDurability reports whether the CURRENT schema carries 0025's
// durability column.
//
// Introspected rather than assumed, for the same reason seedHost checks for
// failure_domain: the tests that pin an EARLIER migration version seed
// fleet_resources against a schema where this column does not exist, so the
// insert must be chosen from the schema that is actually applied rather than from
// the newest one. Where it DOES exist it is NOT NULL with no default — every
// writer must state the retention promise, which is the whole point of D-202.
func hasResourceDurability(t *testing.T, db *gorm.DB) bool {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'fleet_resources' AND column_name = 'durability'`).Scan(&n).Error; err != nil {
		t.Fatalf("introspect fleet_resources.durability: %v", err)
	}
	return n > 0
}

// seedDurableResource inserts a live, cadence-enrolled dedicated claim.
func seedDurableResource(t *testing.T, db *gorm.DB, cadence *int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation,
		 durability, protection_cadence_seconds, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'ready', 1, 'durable', ?, ?, ?)`,
		id, uuid.New(), "claim-"+uuid.NewString(), cadence, now, now).Error
	if err != nil {
		t.Fatalf("seed durable resource: %v", err)
	}
	return id
}

// seedClaimVolume inserts a volume OWNED by a claim and pinned to a host.
func seedClaimVolume(t *testing.T, db *gorm.DB, resourceID uuid.UUID, host *uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO fleet_volumes
		(id, app_id, resource_id, size_mb, host_affinity, mount_path, status, device_name, created_at, updated_at)
		VALUES (?, NULL, ?, 1024, ?, ?, 'available', '/dev/vdb', ?, ?)`,
		uuid.New(), resourceID, host, "/data-"+uuid.NewString()[:8], now, now).Error
	if err != nil {
		t.Fatalf("seed claim volume: %v", err)
	}
}

// ⚠ THE MIGRATION REFUSES A TIER IT DOES NOT KNOW. Silently stamping `ephemeral`
// — the weaker promise — onto a row whose tier this build has never seen is
// exactly the guess that makes a retention promise unrecoverable.
func TestMigration0025RefusesAnUnknownTier(t *testing.T) {
	db, m := startLeasePG(t)
	if err := m.Migrate(24); err != nil {
		t.Fatalf("migrate to 24: %v", err)
	}
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, created_at, updated_at)
		VALUES (?, ?, 'legacy', 'prod', 1, 'postgres', 'serverless-v0', 'ready', 1, ?, ?)`,
		uuid.New(), uuid.New(), now, now).Error; err != nil {
		t.Fatalf("seed unknown-tier row: %v", err)
	}

	err := m.Migrate(25)
	if err == nil {
		t.Fatal("0025 must REFUSE to backfill durability over a tier it does not know")
	}
	if !strings.Contains(err.Error(), "serverless-v0") {
		t.Fatalf("the refusal must name the offending tier: %v", err)
	}
}

// The durability column is stored AND enforced: the two combinations the platform
// cannot hold are unrepresentable, not merely refused in Go.
func TestMigration0025DurabilityIsEnforcedRelationally(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	now := time.Now().UTC()
	insert := func(tier, durability string) error {
		return db.Exec(`INSERT INTO fleet_resources
			(id, owner_org, claim_key, env, revision, class, tier, phase, generation, durability, created_at, updated_at)
			VALUES (?, ?, ?, 'prod', 1, 'postgres', ?, 'ready', 1, ?, ?, ?)`,
			uuid.New(), uuid.New(), "claim-"+uuid.NewString(), tier, durability, now, now).Error
	}
	if err := insert("dedicated", "durable"); err != nil {
		t.Fatalf("a durable dedicated claim must be storable: %v", err)
	}
	if err := insert("shared", "ephemeral"); err != nil {
		t.Fatalf("an ephemeral shared claim must be storable: %v", err)
	}
	if err := insert("dedicated", "ephemeral"); err == nil {
		t.Fatal("dedicated/ephemeral must be UNREPRESENTABLE — the dedicated tier is durable")
	}
	if err := insert("shared", "durable"); err == nil {
		t.Fatal("shared/durable must be UNREPRESENTABLE — a TTL-reaped logical database is not durable")
	}
	if err := insert("dedicated", "sort-of"); err == nil {
		t.Fatal("a durability outside the vocabulary must be refused")
	}
}

// The waiver audit is who + why + when, ALL THREE OR NONE.
func TestMigration0025WaiverAuditIsAllOrNone(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	id := seedDurableResource(t, db, nil)

	set := func(by, reason string, at any) error {
		return db.Exec(`UPDATE fleet_resources
			SET protection_waived_by = ?, protection_waiver_reason = ?, protection_waived_at = ?
			WHERE id = ?`, by, reason, at, id).Error
	}
	now := time.Now().UTC()
	if err := set("user:ops-1", "D-205 drill", now); err != nil {
		t.Fatalf("a complete waiver must be storable: %v", err)
	}
	if err := set("", "", nil); err != nil {
		t.Fatalf("no waiver at all must be storable: %v", err)
	}
	if err := set("user:ops-1", "", nil); err == nil {
		t.Fatal("an actor with no reason is not an audit record and must be refused")
	}
	if err := set("", "D-205 drill", nil); err == nil {
		t.Fatal("a reason attributable to nobody must be refused")
	}
	if err := set("user:ops-1", "D-205 drill", nil); err == nil {
		t.Fatal("an untimed waiver must be refused")
	}
}

// A zero cadence is "no cadence" wearing a number.
func TestMigration0025RefusesAZeroCadence(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	id := seedDurableResource(t, db, nil)

	if err := db.Exec(`UPDATE fleet_resources SET protection_cadence_seconds = 0 WHERE id = ?`, id).Error; err == nil {
		t.Fatal("a cadence of 0 must be refused — NULL is how 'not enrolled' is said")
	}
	if err := db.Exec(`UPDATE fleet_resources SET protection_cadence_seconds = 3600 WHERE id = ?`, id).Error; err != nil {
		t.Fatalf("a positive cadence must be storable: %v", err)
	}
}

// ⚠ THE SCOPE-SHAPE CHECK IS LOAD-BEARING. It forbids a GLOBAL cadence row (one
// host's liveness greening every host's accepts — the cross-host false positive)
// and forbids a SCOPED offsite row (a per-host beat impersonating a platform
// capability).
func TestMigration0025HeartbeatScopeShapeIsEnforced(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	now := time.Now().UTC()
	insert := func(component, scope string) error {
		return db.Exec(`INSERT INTO fleet_protection_heartbeats (component, scope, beaten_at, detail)
			VALUES (?, ?, ?, '')`, component, scope, now).Error
	}
	if err := insert("offsite", ""); err != nil {
		t.Fatalf("the platform offsite row must be storable: %v", err)
	}
	if err := insert("cadence", uuid.NewString()); err != nil {
		t.Fatalf("a host-scoped cadence row must be storable: %v", err)
	}
	if err := insert("cadence", ""); err == nil {
		t.Fatal("a GLOBAL cadence row must be refused — one host's beat must never green another host's data")
	}
	if err := insert("offsite", uuid.NewString()); err == nil {
		t.Fatal("a SCOPED offsite row must be refused — the durability store is a platform-wide fact")
	}
	if err := insert("mirror", ""); err == nil {
		t.Fatal("a component outside the vocabulary must be refused")
	}
}

// The heartbeat is ONE row per (component, scope): two passes never accumulate,
// and a get of a never-beaten component is the ordinary absent fact.
func TestProtectionHeartbeatRoundTrip(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewFleetResourceRepository(db)
	ctx := context.Background()
	host := uuid.NewString()

	if _, err := repo.GetProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, host); !errors.Is(err, domain.ErrProtectionHeartbeatNotFound) {
		t.Fatalf("a never-beaten component must answer ErrProtectionHeartbeatNotFound, got %v", err)
	}

	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	second := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.UpsertProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, host, first, "pass-1"); err != nil {
		t.Fatalf("first beat: %v", err)
	}
	if err := repo.UpsertProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, host, second, "pass-2"); err != nil {
		t.Fatalf("second beat: %v", err)
	}
	var rows int64
	if err := db.Raw(`SELECT count(*) FROM fleet_protection_heartbeats WHERE component = 'cadence' AND scope = ?`, host).Scan(&rows).Error; err != nil {
		t.Fatalf("count beats: %v", err)
	}
	if rows != 1 {
		t.Fatalf("two passes wrote %d rows, want exactly 1 — the beat is a fact, not a log", rows)
	}
	beat, err := repo.GetProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, host)
	if err != nil {
		t.Fatalf("get beat: %v", err)
	}
	if !beat.BeatenAt.UTC().Equal(second) || beat.Detail != "pass-2" {
		t.Fatalf("beat = %v/%q, want the NEWEST pass %v/pass-2", beat.BeatenAt.UTC(), beat.Detail, second)
	}

	// Another host's beat is a different fact and does not resolve here.
	if _, err := repo.GetProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, uuid.NewString()); !errors.Is(err, domain.ErrProtectionHeartbeatNotFound) {
		t.Fatalf("another host's scope must not resolve, got %v", err)
	}
}

// The cadence work list, host-scoped by the CLAIM-OWNED volumes' affinity and by
// nothing else.
func TestListResourcesDueSnapshot(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewFleetResourceRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()

	selfHost := uuid.New()
	otherHost := uuid.New()
	seedHost(t, db, selfHost, now)
	seedHost(t, db, otherHost, now)

	hourly := 3600
	// due: enrolled, pinned here, last success older than its own cadence.
	due := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, due, &selfHost)
	mustExec(t, db, `UPDATE fleet_resources SET last_snapshot_success_at = ? WHERE id = ?`, now.Add(-2*time.Hour), due)

	// never snapshotted: due, and ordered FIRST (NULLS FIRST).
	never := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, never, &selfHost)

	// fresh success: not due.
	fresh := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, fresh, &selfHost)
	mustExec(t, db, `UPDATE fleet_resources SET last_snapshot_success_at = ? WHERE id = ?`, now.Add(-time.Minute), fresh)

	// recent failure: on cooldown.
	cooling := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, cooling, &selfHost)
	mustExec(t, db, `UPDATE fleet_resources SET last_snapshot_failure_at = ? WHERE id = ?`, now.Add(-time.Minute), cooling)

	// restoring: excluded — a snapshot would freeze the volume the restore is
	// swapping underneath it.
	restoring := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, restoring, &selfHost)
	mustExec(t, db, `UPDATE fleet_resources SET phase = 'restoring' WHERE id = ?`, restoring)

	// tombstoned: excluded.
	tomb := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, tomb, &selfHost)
	mustExec(t, db, `UPDATE fleet_resources SET decommissioned_at = ? WHERE id = ?`, now, tomb)

	// not enrolled: excluded.
	unenrolled := seedDurableResource(t, db, nil)
	seedClaimVolume(t, db, unenrolled, &selfHost)

	// pinned to ANOTHER host: excluded — this host cannot freeze a file that is
	// not on its filesystem.
	elsewhere := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, elsewhere, &otherHost)

	// pinned to BOTH: excluded — no single worker protects it.
	split := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, split, &selfHost)
	seedClaimVolume(t, db, split, &otherHost)

	// unpinned: excluded — nothing says where the bytes are.
	unpinned := seedDurableResource(t, db, &hourly)
	seedClaimVolume(t, db, unpinned, nil)

	// owns no volume at all: excluded — nothing to snapshot.
	volumeless := seedDurableResource(t, db, &hourly)

	// Every resource above needs a backing app for the worker to capture.
	mustExec(t, db, `UPDATE fleet_resources SET app_id = ? WHERE app_id IS NULL`, uuid.New())

	got, err := repo.ListResourcesDueSnapshot(ctx, selfHost, now, 10*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListResourcesDueSnapshot: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(got))
	for i := range got {
		ids = append(ids, got[i].ID)
	}
	if len(ids) != 2 || ids[0] != never || ids[1] != due {
		t.Fatalf("work list = %v, want exactly [%s (never snapshotted, first), %s (stale success)]", ids, never, due)
	}
	for _, excluded := range []uuid.UUID{fresh, cooling, restoring, tomb, unenrolled, elsewhere, split, unpinned, volumeless} {
		for _, id := range ids {
			if id == excluded {
				t.Fatalf("resource %s must be excluded from this host's work list", excluded)
			}
		}
	}

	// The other host's work list is its own.
	other, err := repo.ListResourcesDueSnapshot(ctx, otherHost, now, 10*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListResourcesDueSnapshot(other): %v", err)
	}
	for i := range other {
		if other[i].ID != elsewhere {
			t.Fatalf("the other host's work list contains %s, want only %s", other[i].ID, elsewhere)
		}
	}

	// The batch bound is honoured: each capture freezes a customer's guest.
	capped, err := repo.ListResourcesDueSnapshot(ctx, selfHost, now, 10*time.Minute, 1)
	if err != nil {
		t.Fatalf("ListResourcesDueSnapshot(limit 1): %v", err)
	}
	if len(capped) != 1 || capped[0].ID != never {
		t.Fatalf("limited work list = %v, want the oldest-due one only", capped)
	}
}

// ⚠ THE ROLLBACK REFUSES WHILE ANY WAIVER AUDIT EXISTS. Everything else 0025 adds
// is re-derivable; the record of WHO accepted an unprotected durable database and
// WHY is not, and a rollback must never manufacture reversibility by destroying
// it.
func TestMigration0025DownRefusesToDestroyAWaiverAudit(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	// With no waiver on record the down is clean, and the up re-applies (§24).
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down over a waiver-free ledger must succeed: %v", err)
	}
	var hasColumn int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'fleet_resources' AND column_name = 'durability'`).Scan(&hasColumn).Error; err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if hasColumn != 0 {
		t.Fatal("after down: fleet_resources.durability must be gone")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}

	// Now record a waiver and prove the down REFUSES.
	id := seedDurableResource(t, db, nil)
	mustExec(t, db, `UPDATE fleet_resources
		SET protection_waived_by = 'user:ops-1', protection_waiver_reason = 'D-205 drill', protection_waived_at = now()
		WHERE id = ?`, id)

	err := m.Steps(-1)
	if err == nil {
		t.Fatal("the down must REFUSE while a protection waiver audit exists")
	}
	if !strings.Contains(err.Error(), "waiver") {
		t.Fatalf("the refusal must say why: %v", err)
	}
}

func mustExec(t *testing.T, db *gorm.DB, sql string, args ...any) {
	t.Helper()
	if err := db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
