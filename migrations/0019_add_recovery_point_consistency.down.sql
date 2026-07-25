-- Reverse of 0019_add_recovery_point_consistency.up.sql.

ALTER TABLE fleet_resource_recovery_points
    DROP COLUMN IF EXISTS consistency;
