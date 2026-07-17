-- CP4.5 §9 #5 — P21 network membership on the app (D-164).
-- system_id binds an app to a fleet network (fleet_networks.system_id + env). It
-- is the opaque scope key delivery resolves from catalog; the fleet compares it
-- and never dereferences it (there is no `systems` table).
-- Additive, NOT NULL DEFAULT '' → existing rows stay valid (squawk-safe: constant
-- default, no rewrite). '' = no network membership = the app reaches NO fleet peer
-- (the SNT-XVM terminal DROP governs) — exactly the pre-#5 behavior, so back-compat
-- and fail-closed are the same path.

ALTER TABLE fleet_apps ADD COLUMN system_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS fleet_apps_system_id_idx ON fleet_apps (system_id);
