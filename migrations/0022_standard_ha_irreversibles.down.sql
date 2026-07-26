-- Reverse of 0022_standard_ha_irreversibles.up.sql.
--
-- ⚠ Lossy for one fact that cannot be recovered: dropping failure_domain destroys
-- HUMAN-SUPPLIED knowledge about buildings, breakers and switches that no query
-- can reconstruct. That is acceptable only while the fleet is one machine and no
-- resource claims HA (the window slice 0 exists to land inside). Once a second
-- host is attested, the reversal for a bad change is a new forward migration, not
-- this file.
--
-- Dropped in reverse dependency order: the three tables reference fleet_resources
-- and fleet_hosts, so they go before the columns those tables' constraints were
-- added alongside.

DROP TABLE IF EXISTS failover_events;
DROP TABLE IF EXISTS fleet_resource_leases;

DROP INDEX IF EXISTS fleet_resource_members_one_primary;
DROP TABLE IF EXISTS fleet_resource_members;

ALTER TABLE fleet_resources DROP CONSTRAINT IF EXISTS fleet_resources_sync_degrade_policy_ck;
ALTER TABLE fleet_resources DROP COLUMN IF EXISTS sync_degrade_policy;
ALTER TABLE fleet_resources DROP CONSTRAINT IF EXISTS fleet_resources_availability_class_ck;
ALTER TABLE fleet_resources DROP COLUMN IF EXISTS availability_class;

ALTER TABLE fleet_hosts DROP CONSTRAINT IF EXISTS fleet_hosts_region_ck;
ALTER TABLE fleet_hosts DROP CONSTRAINT IF EXISTS fleet_hosts_failure_domain_ck;
ALTER TABLE fleet_hosts DROP COLUMN IF EXISTS failure_domain;
