-- D-203 — re-parent fleet_volumes: the claim owns, nothing cascades.
--
-- Before this migration, fleet_volumes.app_id was NOT NULL ON DELETE CASCADE
-- (0001:89): any fleet_apps delete destroyed the volume ledger — the durable
-- record of where a customer's bytes live — with no verb, no snapshot check
-- and no audit. Ownership by the durable claim (fleet_resources) existed only
-- as a reverse join one Go guard performed. This migration makes the database
-- itself refuse what the guard refuses:
--
--   * resource_id  — nullable FK RESTRICT. When set, the claim OWNS the volume.
--   * app_id       — becomes a nullable ATTACHMENT, CASCADE -> RESTRICT.
--   * CHECK        — at least one parent always (an orphan row is bytes nobody
--                    can attribute). Nullable resource_id, not NOT NULL: plain
--                    stateful apps hold volumes with no claim (D-203's own
--                    correction), so a claim-only owner cannot hold.
--   * host_affinity SET NULL -> RESTRICT: silently erasing where the bytes
--                    ARE is the worst available failure mode for a locality
--                    fact. Deleting a host row with pinned volumes is refused.
--   * pool_guid    — the pool-GUID location primitive's column shape, decided
--                    NOW so this table is never migrated twice (D-203). A ZFS
--                    pool GUID is an unsigned 64-bit integer printed decimal;
--                    TEXT + a digits CHECK holds the full range (BIGINT is
--                    signed and would reinterpret the upper half). The CHECK
--                    also bounds the value at 2^64-1: digits alone admit
--                    20-digit values past the uint64 range. NULL means
--                    "location not pool-attested" — true of every ext4-file-era
--                    volume. No writer yet; the writer arrives with the
--                    host-identity ruling, and a future fleet_storage_pools FK
--                    is additive over this same column.
--
-- Deletion of volume rows is now possible ONLY through explicit verbs
-- (FleetVolumeManager.DeleteAppVolumes, itself claim-guarded in Go).
--
-- Locking + squawk: every DDL statement here takes a brief lock on
-- fleet_volumes; the table has exactly ONE row (live-verified 2026-07-26) and
-- one single-writer control plane, so the whole change runs as ONE explicit
-- transaction — schema, backfill and constraints land atomically or not at
-- all, and a mid-file failure leaves nothing to repair. lock_timeout bounds
-- every lock wait so a stuck autovacuum makes this fail fast rather than
-- queue behind it. The squawk-ignore lines below are line-scoped and each
-- suppresses a lock-avoidance rule (NOT VALID/VALIDATE two-phase, CONCURRENTLY)
-- whose cost — losing atomicity across migration files — buys nothing on a
-- one-row single-writer table; §24 itself requires CONCURRENTLY only above
-- 100k rows. ban-drop-not-null is the intended semantic, not an accident.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

ALTER TABLE fleet_volumes ADD COLUMN IF NOT EXISTS resource_id UUID;
ALTER TABLE fleet_volumes ADD COLUMN IF NOT EXISTS pool_guid TEXT;
-- squawk-ignore ban-drop-not-null
ALTER TABLE fleet_volumes ALTER COLUMN app_id DROP NOT NULL;

-- Backfill: stamp every volume whose app a LIVE claim backs. Deterministic on
-- purpose (oldest live claim per app wins) so a duplicate-claim anomaly could
-- never make this migration's result depend on join order. Live data at
-- migration time: exactly one volume (63a041bf-…), whose app is backed by live
-- resource 111a8178-… — that row and only that row is stamped.
UPDATE fleet_volumes v
SET resource_id = r.id
FROM (
    SELECT DISTINCT ON (app_id) id, app_id
    FROM fleet_resources
    WHERE decommissioned_at IS NULL AND app_id IS NOT NULL
    ORDER BY app_id, created_at ASC, id ASC
) r
WHERE v.app_id = r.app_id
  AND v.resource_id IS NULL;

-- FK swaps. Constraint names are the live ones (verified via pg_constraint).
-- Plain (validated) ADD CONSTRAINT on purpose: inside this transaction the
-- swap is atomic — there is never a moment where the old CASCADE is gone and
-- the new RESTRICT is absent or unvalidated. The scan it implies reads one row.
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_app_id_fkey;
-- squawk-ignore adding-foreign-key-constraint,constraint-missing-not-valid
ALTER TABLE fleet_volumes ADD CONSTRAINT fleet_volumes_app_id_fkey FOREIGN KEY (app_id) REFERENCES fleet_apps(id) ON DELETE RESTRICT;

ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_host_affinity_fkey;
-- squawk-ignore adding-foreign-key-constraint,constraint-missing-not-valid
ALTER TABLE fleet_volumes ADD CONSTRAINT fleet_volumes_host_affinity_fkey FOREIGN KEY (host_affinity) REFERENCES fleet_hosts(id) ON DELETE RESTRICT;

-- squawk-ignore adding-foreign-key-constraint,constraint-missing-not-valid
ALTER TABLE fleet_volumes ADD CONSTRAINT fleet_volumes_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES fleet_resources(id) ON DELETE RESTRICT;

-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_volumes ADD CONSTRAINT fleet_volumes_owner_present_ck CHECK (resource_id IS NOT NULL OR app_id IS NOT NULL);

-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_volumes ADD CONSTRAINT fleet_volumes_pool_guid_ck CHECK (pool_guid IS NULL OR (pool_guid ~ '^[0-9]{1,20}$' AND pool_guid::numeric <= 18446744073709551615::numeric));

-- Plain CREATE INDEX inside the transaction, not CONCURRENTLY: §24 requires
-- CONCURRENTLY only on tables >100k rows; here it would force a separate
-- non-transactional file and can strand an INVALID index on failure.
-- squawk-ignore require-concurrent-index-creation
CREATE INDEX IF NOT EXISTS fleet_volumes_resource_id_idx ON fleet_volumes (resource_id);

COMMIT;
