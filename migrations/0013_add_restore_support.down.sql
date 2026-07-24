-- Reverse of 0013_add_restore_support.up.sql.

ALTER TABLE fleet_resource_recovery_points DROP COLUMN IF EXISTS checksum;
ALTER TABLE fleet_resources DROP COLUMN IF EXISTS last_error;
