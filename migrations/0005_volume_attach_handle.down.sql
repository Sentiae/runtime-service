-- Reverse of 0005_volume_attach_handle.up.sql.

ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS device_name;
ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS status;
ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS attached_replica;
ALTER TABLE fleet_volumes DROP COLUMN IF EXISTS backing_path;
