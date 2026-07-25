-- Reverse of 0021_add_resource_endpoint_identity.up.sql.
--
-- ⚠ Lossy by nature: dropping endpoint_id destroys minted PERMANENT names. That
-- is acceptable only while no customer holds a connection string (the window
-- D-190 exists to land inside) — after that, this down migration is not a
-- rollback, it is data loss, and the reversal for a bad change is a new forward
-- migration instead.

DROP INDEX IF EXISTS fleet_resources_endpoint_id_key;

ALTER TABLE fleet_resources DROP CONSTRAINT IF EXISTS fleet_resources_endpoint_id_ck;
ALTER TABLE fleet_resources DROP CONSTRAINT IF EXISTS fleet_resources_generation_ck;

ALTER TABLE fleet_resources DROP COLUMN IF EXISTS generation;
ALTER TABLE fleet_resources DROP COLUMN IF EXISTS region;
ALTER TABLE fleet_resources DROP COLUMN IF EXISTS endpoint_id;
