-- Reverse of 0003_fleet_app_secret_refs.up.sql.

ALTER TABLE fleet_apps DROP COLUMN IF EXISTS secret_refs;
