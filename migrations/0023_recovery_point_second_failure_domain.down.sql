-- Reverse of 0023_recovery_point_second_failure_domain.up.sql.
--
-- ⚠ Lossy in the direction that matters: dropping these columns discards the ONLY
-- record of which recovery points exist off the chassis, and it cannot be
-- reconstructed (the second bucket is not listable — D-199). Reversible as DDL,
-- not as knowledge.

DROP INDEX IF EXISTS fleet_resource_recovery_points_single_domain_idx;

ALTER TABLE fleet_resource_recovery_points
    DROP CONSTRAINT IF EXISTS fleet_resource_recovery_points_second_domain_ck;

ALTER TABLE fleet_resource_recovery_points
    DROP CONSTRAINT IF EXISTS fleet_resource_recovery_points_locations_ck;

ALTER TABLE fleet_resource_recovery_points
    DROP COLUMN IF EXISTS second_domain_error,
    DROP COLUMN IF EXISTS second_domain_at,
    DROP COLUMN IF EXISTS second_domain_store,
    DROP COLUMN IF EXISTS locations;
