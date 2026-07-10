-- runtime-fleet P3.4 — the attested owner org anchor (D-069).
-- Carries the tenant (org uuid) that owns a resident app's secrets so the
-- replica runtime scopes every secret_ref resolution to it (I28): secrets
-- resolve only under this org's per-tenant KEK. Additive, NOT NULL DEFAULT ''
-- → existing rows stay valid (squawk-safe: no lock-heavy backfill, default is
-- constant).

ALTER TABLE fleet_apps ADD COLUMN owner_org TEXT NOT NULL DEFAULT '';
