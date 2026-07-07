-- Reverse of 0001_fleet_control_plane.up.sql — drop in reverse FK order.

DROP TABLE IF EXISTS fleet_secret_bindings;
DROP TABLE IF EXISTS fleet_volumes;
DROP TABLE IF EXISTS fleet_routes;
DROP TABLE IF EXISTS fleet_placements;
DROP TABLE IF EXISTS fleet_replicas;
DROP TABLE IF EXISTS fleet_apps;
DROP TABLE IF EXISTS fleet_hosts;
