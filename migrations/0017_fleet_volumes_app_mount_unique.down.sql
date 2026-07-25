-- Reverse 0017: drop the (app_id, mount_path) uniqueness index. CONCURRENTLY +
-- single statement so it runs outside a transaction block (matching the up).
DROP INDEX CONCURRENTLY IF EXISTS fleet_volumes_app_mount_key;
