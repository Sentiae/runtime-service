-- Reverses 0020. Dropping the lease table gives up the durable allocation state,
-- so a host rolled back here is back to the in-memory allocator: the down
-- migration is for a clean rollback of an unreleased deploy, not something to run
-- against a host with live microVMs.

DROP TABLE IF EXISTS fleet_net_leases;

ALTER TABLE fleet_hosts DROP CONSTRAINT IF EXISTS fleet_hosts_net_ordinal_ck;
DROP INDEX IF EXISTS fleet_hosts_net_ordinal_key;
ALTER TABLE fleet_hosts DROP COLUMN IF EXISTS net_ordinal;
