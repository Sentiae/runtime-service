-- Reverse of 0002_replica_runtime_handle.up.sql.

ALTER TABLE fleet_replicas DROP COLUMN IF EXISTS port;
ALTER TABLE fleet_replicas DROP COLUMN IF EXISTS tap_name;
ALTER TABLE fleet_replicas DROP COLUMN IF EXISTS socket_path;
ALTER TABLE fleet_replicas DROP COLUMN IF EXISTS rootfs_path;
