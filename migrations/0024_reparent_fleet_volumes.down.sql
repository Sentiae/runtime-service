-- Reverses 0024. Rows representable ONLY under the new shape (app_id IS NULL —
-- creatable solely by post-0024 verbs) have no v23 representation: this
-- rollback REFUSES while any exist rather than deleting them. Every pre-0024
-- row is untouched. One explicit transaction, mirroring the up: the restore of
-- NOT NULL + CASCADE + SET NULL either fully lands or nothing changes — a
-- half-reverted ledger schema is worse than either shape.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

DROP INDEX IF EXISTS fleet_volumes_resource_id_idx;
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_owner_present_ck;
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_resource_id_fkey;

ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_host_affinity_fkey;
ALTER TABLE fleet_volumes
    ADD CONSTRAINT fleet_volumes_host_affinity_fkey
    FOREIGN KEY (host_affinity) REFERENCES fleet_hosts(id) ON DELETE SET NULL;

-- A ledger rollback must refuse, never manufacture reversibility by deleting
-- rows. Claim-only rows (app_id IS NULL) have no v23 representation; if any
-- exist, recovery is forward — a new migration or restore — not destruction.
DO $$
DECLARE claim_only bigint;
BEGIN
    SELECT count(*) INTO claim_only FROM fleet_volumes WHERE app_id IS NULL;
    IF claim_only > 0 THEN
        RAISE EXCEPTION 'refusing rollback: % claim-only fleet_volumes rows (app_id IS NULL) cannot be represented in the v23 schema — recover forward or restore from a verified backup', claim_only;
    END IF;
END $$;

ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_app_id_fkey;
ALTER TABLE fleet_volumes ALTER COLUMN app_id SET NOT NULL;
ALTER TABLE fleet_volumes
    ADD CONSTRAINT fleet_volumes_app_id_fkey
    FOREIGN KEY (app_id) REFERENCES fleet_apps(id) ON DELETE CASCADE;

ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS resource_id;

COMMIT;
