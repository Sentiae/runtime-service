-- Reverses 0025. One explicit transaction, mirroring the up: either the v24 shape
-- is fully restored or nothing changes — a half-reverted ledger schema is worse
-- than either shape.
--
-- ⚠ IT REFUSES WHILE ANY WAIVER AUDIT EXISTS (J2). durability, the cadence
-- enrolment, protection_attached_at and the heartbeat rows are all DERIVABLE
-- again: the first from tier, the second from configuration at the next accept,
-- the third from a re-attach, the fourth from the next pass of the workers that
-- write them. A waiver record is not. It is the permanent, per-resource statement
-- of WHO accepted a durable database the platform could not protect and WHY —
-- the only override D-202 admits, and the whole reason the override is allowed to
-- exist. Dropping those columns would destroy an audit record with no other copy,
-- so this rollback refuses and recovery is forward (a new migration, or a restore
-- from a verified backup), exactly as 0024 refuses to delete claim-only volumes.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

DO $$
DECLARE waived bigint;
BEGIN
    SELECT count(*) INTO waived
    FROM fleet_resources
    WHERE protection_waived_by <> ''
       OR protection_waiver_reason <> ''
       OR protection_waived_at IS NOT NULL;
    IF waived > 0 THEN
        RAISE EXCEPTION 'refusing rollback: % fleet_resources row(s) carry a D-202 protection waiver audit (who/why/when) that has no v24 representation and cannot be re-derived — recover forward or restore from a verified backup', waived;
    END IF;
END $$;

DROP TABLE IF EXISTS fleet_protection_heartbeats;

DROP INDEX IF EXISTS fleet_resources_cadence_due_idx;

ALTER TABLE fleet_resources
    DROP CONSTRAINT IF EXISTS fleet_resources_protection_waiver_ck,
    DROP CONSTRAINT IF EXISTS fleet_resources_protection_cadence_ck,
    DROP COLUMN IF EXISTS protection_waived_at,
    DROP COLUMN IF EXISTS protection_waiver_reason,
    DROP COLUMN IF EXISTS protection_waived_by,
    DROP COLUMN IF EXISTS protection_attached_at,
    DROP COLUMN IF EXISTS protection_cadence_seconds;

ALTER TABLE fleet_resources
    DROP CONSTRAINT IF EXISTS fleet_resources_tier_durability_ck,
    DROP CONSTRAINT IF EXISTS fleet_resources_durability_ck,
    DROP COLUMN IF EXISTS durability;

COMMIT;
