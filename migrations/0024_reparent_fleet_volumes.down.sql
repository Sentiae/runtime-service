-- Reverses 0024. Rows representable ONLY under the new shape (app_id IS NULL —
-- creatable solely by post-0024 verbs, none of which exist at 0024 time) cannot
-- satisfy the restored NOT NULL and are deleted; every pre-0024 row is
-- untouched. resource_id / pool_guid data is discarded with the columns —
-- that IS the reversal. One explicit transaction, mirroring the up: the
-- restore of NOT NULL + CASCADE + SET NULL either fully lands or nothing
-- changes — a half-reverted ledger schema is worse than either shape.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

DROP INDEX IF EXISTS fleet_volumes_resource_id_idx;
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_pool_guid_ck;
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_owner_present_ck;
ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_resource_id_fkey;

ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_host_affinity_fkey;
ALTER TABLE fleet_volumes
    ADD CONSTRAINT fleet_volumes_host_affinity_fkey
    FOREIGN KEY (host_affinity) REFERENCES fleet_hosts(id) ON DELETE SET NULL;

DELETE FROM fleet_volumes WHERE app_id IS NULL;

ALTER TABLE fleet_volumes DROP CONSTRAINT IF EXISTS fleet_volumes_app_id_fkey;
ALTER TABLE fleet_volumes ALTER COLUMN app_id SET NOT NULL;
ALTER TABLE fleet_volumes
    ADD CONSTRAINT fleet_volumes_app_id_fkey
    FOREIGN KEY (app_id) REFERENCES fleet_apps(id) ON DELETE CASCADE;

ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS pool_guid;
ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS resource_id;

COMMIT;
