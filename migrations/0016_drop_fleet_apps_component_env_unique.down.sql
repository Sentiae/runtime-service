-- Reverse of 0016_drop_fleet_apps_component_env_unique.up.sql — restore the
-- org-blind uniqueness.
--
-- ⚠ This reversal is only valid while no two organisations share a
-- (component_id, env) pair. That holds today and is DELIBERATELY false once the
-- fixed code has been exercised: the whole point of the fix is to let two orgs own
-- the same key, and re-adding this constraint then fails on duplicate rows. That is
-- the correct semantics for reversing a tenancy widening — narrowing back is a data
-- decision, not something a migration may silently resolve.

ALTER TABLE fleet_apps ADD CONSTRAINT fleet_apps_component_id_env_key UNIQUE (component_id, env);
