-- runtime-fleet P3.3 — resident app desired secret refs.
-- Carries the app's secret_refs on its desired state so the reconciler re-boots
-- every replica WITH its secret intent (crash-recovery, scale) — this is what
-- lets the host->guest vsock secret channel (invariant I32) fire on the live
-- resident-orchestrator path, not just the CP3 test/fallback path. Additive,
-- NOT NULL DEFAULT '[]' → existing rows stay valid (squawk-safe).

ALTER TABLE fleet_apps ADD COLUMN secret_refs JSONB NOT NULL DEFAULT '[]';
