-- SentiaeDB — fix `#two-orgs-same-claim-key-share-one-database` (cross-tenant).
-- The org-blind (component_id, env) uniqueness IS the defect: it forced two
-- organisations claiming the same key onto one app row (one VM, one volume, one
-- Postgres). 0014 already installed the org-scoped replacement, so dropping this is
-- the last step and leaves the table continuously guarded.
--
-- Two names because the deployed fleet host also runs GORM AutoMigrate
-- (APP_DATABASE_AUTO_MIGRATE=true), which creates its own twin index alongside the
-- Postgres-named constraint from 0001.

ALTER TABLE fleet_apps DROP CONSTRAINT IF EXISTS fleet_apps_component_id_env_key;

DROP INDEX IF EXISTS uq_fleet_apps_component_env;
