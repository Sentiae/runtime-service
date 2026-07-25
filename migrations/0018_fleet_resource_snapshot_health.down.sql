-- Reverse of 0018_fleet_resource_snapshot_health.up.sql.

ALTER TABLE fleet_resources
    DROP COLUMN IF EXISTS last_snapshot_success_at,
    DROP COLUMN IF EXISTS last_snapshot_error,
    DROP COLUMN IF EXISTS last_snapshot_failure_at,
    DROP COLUMN IF EXISTS consecutive_snapshot_failures;
