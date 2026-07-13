-- runtime-fleet CP4 rt#11 — scale-to-zero activity tracking (D-082).
-- fleet_apps gains the idle clock the reconciler's SweepIdle reads: last_active_at
-- is stamped at provision (DEFAULT now()) and refreshed by the activator on each
-- wake; idle_ttl_seconds is the inactivity window before scale-to-zero (0 = off).
-- Both are NOT NULL DEFAULT so existing rows stay valid (squawk-safe additive DDL).
-- No snapshot columns — warm-resume is deferred (D-082 §2).

ALTER TABLE fleet_apps ADD COLUMN last_active_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE fleet_apps ADD COLUMN idle_ttl_seconds INT NOT NULL DEFAULT 0;
