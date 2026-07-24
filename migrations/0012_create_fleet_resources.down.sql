-- Reverse of 0012_create_fleet_resources.up.sql — recovery points reference
-- resources, so drop them first.
DROP TABLE IF EXISTS fleet_resource_recovery_points;
DROP TABLE IF EXISTS fleet_resources;
