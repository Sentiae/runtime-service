-- Reverse of 0004_fleet_app_owner_org.up.sql.

ALTER TABLE fleet_apps DROP COLUMN IF EXISTS owner_org;
