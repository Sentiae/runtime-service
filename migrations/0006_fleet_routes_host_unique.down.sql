-- Reverse 0006: drop the host uniqueness index. CONCURRENTLY + single statement
-- so it runs outside a transaction block (matching the up migration).
DROP INDEX CONCURRENTLY IF EXISTS fleet_routes_host_pattern_key;
