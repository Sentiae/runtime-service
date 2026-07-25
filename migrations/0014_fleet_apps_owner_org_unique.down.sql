-- Reverse 0014: drop the org-scoped uniqueness index. CONCURRENTLY + single
-- statement so it runs outside a transaction block (matching the up migration).
DROP INDEX CONCURRENTLY IF EXISTS fleet_apps_component_env_owner_key;
