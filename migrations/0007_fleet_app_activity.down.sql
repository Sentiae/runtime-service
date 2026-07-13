-- Reverse 0007: drop the scale-to-zero activity columns.
ALTER TABLE fleet_apps DROP COLUMN idle_ttl_seconds;
ALTER TABLE fleet_apps DROP COLUMN last_active_at;
