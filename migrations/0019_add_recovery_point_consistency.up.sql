-- SentiaeDB Phase 0 — a recovery point must say HOW it was made consistent.
--
-- fleet_resource_recovery_points.kind says what the artifact IS ('snapshot').
-- Nothing said what it GUARANTEES, and the three things this platform can
-- produce are not interchangeable:
--
--   guest_frozen     the guest filesystem was flushed and frozen for the ENTIRE
--                    capture — a crash-free image, a valid PITR base.
--   detached_clean   no VM attached and the engine had been stopped cleanly.
--   detached_unclean captured with no freeze from a volume whose writer was not
--                    proven to have stopped cleanly — restoring it is restoring a
--                    crashed filesystem. NOT a PITR anchor.
--   unknown          rows written before this column existed.
--
-- This is UNBACKFILLABLE: nothing about a blob in the artifact store reveals
-- whether the filesystem it was read from was frozen at the time. It therefore
-- has to land before any real customer data does — afterwards every pre-existing
-- row is permanently 'unknown', which must be read as the WEAKEST class and never
-- as "probably fine".
--
-- Additive with a constant default (TEXT NOT NULL DEFAULT 'unknown') → no table
-- rewrite, no lock beyond the catalog update (squawk-safe).

ALTER TABLE fleet_resource_recovery_points
    ADD COLUMN consistency TEXT NOT NULL DEFAULT 'unknown';
