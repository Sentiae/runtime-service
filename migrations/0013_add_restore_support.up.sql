-- SentiaeDB Phase 0 (D-184) — the in-place restore path (P19 Restore).
--
-- last_error carries the terminal reason of the last restore attempt so a poller
-- can tell a rolled-back restore from a successful one (both end in phase
-- 'ready') and so a restore interrupted by a service restart is legible.
--
-- checksum makes restore-integrity honest: until now a recovery point recorded
-- only size_bytes, so a corrupt blob was indistinguishable from a good one. The
-- snapshotter computes sha256 while streaming the upload; the restorer verifies
-- it before touching the live volume. Legacy rows keep '' and are size-verified
-- only (the restorer logs that explicitly rather than implying integrity).
--
-- Both are additive TEXT NOT NULL DEFAULT '' → constant default, no table
-- rewrite, no lock beyond the catalog update (squawk-safe).

ALTER TABLE fleet_resources
    ADD COLUMN last_error TEXT NOT NULL DEFAULT '';

ALTER TABLE fleet_resource_recovery_points
    ADD COLUMN checksum TEXT NOT NULL DEFAULT '';
