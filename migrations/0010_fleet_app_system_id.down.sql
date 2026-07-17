-- Reverse of 0010_fleet_app_system_id.up.sql.

DROP INDEX IF EXISTS fleet_apps_system_id_idx;
ALTER TABLE fleet_apps DROP COLUMN IF EXISTS system_id;
