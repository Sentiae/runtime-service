-- runtime-fleet CP4 rt#8 — ingress host uniqueness.
-- The fleet owns ingress (D-079): every route's host_pattern maps to exactly one
-- app, so a host must be unique across fleet_routes. CONCURRENTLY keeps the (tiny
-- but growing) table writable during index build; a single statement so the
-- golang-migrate runner executes it outside a transaction block (CONCURRENTLY
-- cannot run inside one).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_routes_host_pattern_key ON fleet_routes (host_pattern);
