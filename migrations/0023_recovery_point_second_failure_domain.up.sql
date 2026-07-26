-- SentiaeDB Phase 0 — WHERE a recovery point exists must be a recorded fact, not
-- an inference from configuration.
--
-- Until now every recovery point landed in exactly one place: the MinIO container
-- that runs on the SAME PHYSICAL CHASSIS as the fleet host whose data it protects.
-- `failure_domains = 1`, so every durability promise was arithmetic over one
-- machine. D-192/D-195 created the second domain (Cloudflare R2
-- `sentiae-recovery-points`, 30-day object lock over all prefixes) and D-199
-- stored its credential; this migration is what lets the ledger SAY which copies
-- made it there.
--
-- ⚠ THIS IS UNBACKFILLABLE, and that is why it lands WITH the mirroring rather
-- than after it. Nothing about an existing row reveals whether its blob was ever
-- copied off the chassis — and the second bucket cannot be enumerated to find out,
-- because the D-199 token grants object read/write and NOT bucket listing (LIST
-- returns 403, verified live). So every row written before this column is
-- permanently 'unknown', which must be read as the WEAKEST class (NOT two
-- domains), exactly as 0019's consistency='unknown' is.
--
-- ENCODING (frozen — see domain.RecoveryPointLocations):
--
--   unknown                     the row predates this column. Not provably in two
--                               domains, and never to be counted as such.
--   primary_only                the blob is in the primary store and NOWHERE else.
--                               Stamped at capture, before the mirror is attempted,
--                               so a crash between the two never leaves a row
--                               claiming a copy that does not exist.
--   primary_and_second_domain   the second copy was written AND its bytes were
--                               read back and hashed to the recorded checksum.
--
-- ⚠ THE STATE MACHINE IS ONE-WAY: primary_only → primary_and_second_domain, and
-- only on a CONFIRMED, checksum-verified second copy. Nothing may stamp the
-- two-domain value optimistically at capture time and repair it later: an
-- optimistic stamp is a durability claim made before the durability exists, which
-- is the failure this whole column exists to prevent.
--
-- Additive with constant defaults (TEXT NOT NULL DEFAULT, nullable TIMESTAMPTZ) →
-- catalog-only, no table rewrite, squawk-safe.

ALTER TABLE fleet_resource_recovery_points
    ADD COLUMN locations           TEXT NOT NULL DEFAULT 'unknown',
    -- WHICH second domain holds the copy (e.g. 'cloudflare-r2:sentiae-recovery-points').
    -- A bare boolean would not survive a second mirror target, and during a
    -- migration between targets "which bucket is this in" is the only question that
    -- matters. Empty while no second copy is confirmed.
    ADD COLUMN second_domain_store TEXT NOT NULL DEFAULT '',
    -- When the second copy was CONFIRMED (verified, not merely requested). NULL
    -- while there is none.
    ADD COLUMN second_domain_at    TIMESTAMPTZ,
    -- The last failed mirror attempt's cause, for an operator. It is deliberately
    -- NOT folded into fleet_resources.last_snapshot_error: a failed mirror is not a
    -- failed snapshot — a recovery point DOES exist — and counting it as one would
    -- fire the protection-has-stopped alert for a WAN blip while hiding the real
    -- condition, which is that this copy is single-domain.
    ADD COLUMN second_domain_error TEXT NOT NULL DEFAULT '';

-- A vocabulary fence, so the classes cannot rot into free text and so no writer
-- can invent a fourth meaning for "where is this blob".
ALTER TABLE fleet_resource_recovery_points
    ADD CONSTRAINT fleet_resource_recovery_points_locations_ck
    CHECK (locations IN ('unknown', 'primary_only', 'primary_and_second_domain'));

-- The two-domain claim must not be assertable without naming WHERE and WHEN.
-- Without this a row could read 'primary_and_second_domain' with an empty store
-- and a NULL timestamp — a durability claim with no evidence attached, which is
-- indistinguishable from the fail-open it replaces.
ALTER TABLE fleet_resource_recovery_points
    ADD CONSTRAINT fleet_resource_recovery_points_second_domain_ck
    CHECK (
        locations <> 'primary_and_second_domain'
        OR (second_domain_store <> '' AND second_domain_at IS NOT NULL)
    );

-- The metric's read path: count by class and find the OLDEST copy that is not
-- provably in two domains. Partial, because the alarming population is the small
-- one — a healthy fleet has almost every row in the excluded class.
CREATE INDEX fleet_resource_recovery_points_single_domain_idx
    ON fleet_resource_recovery_points (created_at)
    WHERE locations <> 'primary_and_second_domain';
