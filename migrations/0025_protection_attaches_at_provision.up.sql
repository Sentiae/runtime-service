-- D-202 — protection attaches or the provision fails.
-- (base spec docs/program/03-execution/arch/d202-protection-attaches-or-provision-fails.md,
--  as amended by the CONFIRMED-BY-BOTH record d202-joint-refresh.md — J2 governs this file.)
--
-- Three shapes: (1) durability becomes a STORED, ENFORCED column — the promise a
-- resource was accepted under can never again be inferred from tier; (2) the
-- protection attachment lives ON the claim row, written in the same INSERT that
-- creates it, so an unprotected durable acceptance is unrepresentable rather than
-- merely checked; (3) fleet_protection_heartbeats is where the protection workers
-- prove they run — the accept gate reads these rows, NEVER configuration (the
-- deployed D-200 mirror was found ENABLED by config while draining the wrong
-- database; a heartbeat written into the ledger the accept reads cannot make that
-- error).
--
-- Numbering: 0025 is next-free at the moment this branch lands. RunMigrations is
-- golang-migrate m.Up() over the embedded FS, which applies only versions GREATER
-- than the recorded one and performs NO missing-migration detection — deploying a
-- higher number first would permanently skip a lower one (J2).
--
-- Locking + squawk: all four control-plane tables touched here are small (the
-- live ledgers hold single- to double-digit fleet_resources rows) and have one
-- single-writer control plane, so the whole change runs as ONE explicit
-- transaction — schema, refusal, backfill and constraints land atomically or not
-- at all, and a mid-file failure leaves nothing to repair. lock_timeout bounds
-- every lock wait so a stuck autovacuum makes this fail fast rather than queue
-- behind it. The squawk-ignore lines are line-scoped and each suppresses a
-- lock-avoidance rule (NOT VALID two-phase, CONCURRENTLY) whose cost — losing
-- atomicity across migration files — buys nothing at this row count; §24 itself
-- requires CONCURRENTLY only above 100k rows.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- (1) durability — the retention promise, STORED
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Backfilled from the one honest inference available TODAY and never inferable
-- again after: every dedicated row was accepted under the snapshot-first-teardown
-- contract (durable by construction), every shared row is born with expires_at set
-- and is TTL-reaped (ephemeral by construction).
--
-- ⚠ THE MIGRATION REFUSES A TIER IT DOES NOT KNOW (J2). The CASE below would
-- otherwise silently stamp `ephemeral` — the WEAKER promise — onto any row whose
-- tier this build has never seen, which is exactly the class of guess that makes a
-- retention promise unrecoverable. A tier outside the known vocabulary means this
-- migration is running against a ledger written by a build it does not understand,
-- and the honest answer is to stop.
DO $$
DECLARE unknown_tiers text;
BEGIN
    SELECT string_agg(DISTINCT tier, ', ') INTO unknown_tiers
    FROM fleet_resources
    WHERE tier NOT IN ('dedicated', 'shared');
    IF unknown_tiers IS NOT NULL THEN
        RAISE EXCEPTION 'refusing to backfill fleet_resources.durability: unknown tier value(s) [%] — the retention promise of those rows cannot be inferred and must never be guessed (D-202/J2)', unknown_tiers;
    END IF;
END $$;

-- 0022's add-default-then-drop pattern: the transient default covers the ALTER,
-- the backfill corrects it, the drop forces every future writer to state it.
-- Constant default ⇒ catalog-only, no table rewrite (PG11+).
ALTER TABLE fleet_resources ADD COLUMN durability TEXT NOT NULL DEFAULT 'durable';
UPDATE fleet_resources
   SET durability = CASE WHEN tier = 'dedicated' THEN 'durable' ELSE 'ephemeral' END;
ALTER TABLE fleet_resources ALTER COLUMN durability DROP DEFAULT;

-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_resources ADD CONSTRAINT fleet_resources_durability_ck CHECK (durability IN ('durable', 'ephemeral'));

-- The RELATIONAL fence (J2): "stored and enforced" is hollow if the ledger can
-- still represent dedicated/ephemeral or shared/durable. The dedicated tier IS
-- durable (Aurora-shape, not disableable) and a shared logical database is
-- TTL-reaped, so those two combinations are promises the platform cannot hold —
-- unrepresentable here rather than refused only in Go, where one new writer
-- forgets.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_resources ADD CONSTRAINT fleet_resources_tier_durability_ck CHECK ((tier = 'dedicated' AND durability = 'durable') OR (tier = 'shared' AND durability = 'ephemeral'));

-- ─────────────────────────────────────────────────────────────────────────────
-- (2) the attachment, on the claim row itself
-- ─────────────────────────────────────────────────────────────────────────────
--
-- protection_cadence_seconds: NULL = cadence is NOT attached (pre-D-202 rows,
-- ephemeral rows, waived rows where it could not attach). Never 0 — a zero cadence
-- is "no cadence" wearing a number, refused by the CHECK.
-- protection_attached_at: when the FULL component set attached. NULL on waived and
-- pre-existing rows; the status path turns NULL-on-durable into a condition.
--
-- There is deliberately NO protection_capture_mode column here (J2): mode
-- machinery belongs to D-212's migration, added when its writer makes mode
-- behaviour-bearing. A column nothing reads is a reader trap.
-- INT, not BIGINT: this is a period in SECONDS, bounded by the cadence an
-- operator can sanely configure (an hour is 3600). 2^31 seconds is 68 years.
-- squawk-ignore prefer-bigint-over-int
ALTER TABLE fleet_resources ADD COLUMN protection_cadence_seconds INT;
ALTER TABLE fleet_resources ADD COLUMN protection_attached_at TIMESTAMPTZ;
ALTER TABLE fleet_resources ADD COLUMN protection_waived_by TEXT NOT NULL DEFAULT '';
ALTER TABLE fleet_resources ADD COLUMN protection_waiver_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE fleet_resources ADD COLUMN protection_waived_at TIMESTAMPTZ;

-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_resources ADD CONSTRAINT fleet_resources_protection_cadence_ck CHECK (protection_cadence_seconds IS NULL OR protection_cadence_seconds > 0);

-- A waiver is who + why + when, ALL THREE OR NONE: a bare name is not an audit
-- record, a bare reason is not attributable, and an untimed one cannot be aged.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE fleet_resources ADD CONSTRAINT fleet_resources_protection_waiver_ck CHECK ((protection_waived_by = '' AND protection_waiver_reason = '' AND protection_waived_at IS NULL) OR (protection_waived_by <> '' AND protection_waiver_reason <> '' AND protection_waived_at IS NOT NULL));

-- The cadence worker's work-list index: durable rows enrolled in a cadence and not
-- torn down. Partial — the scanning population is the small one.
-- squawk-ignore require-concurrent-index-creation
CREATE INDEX fleet_resources_cadence_due_idx ON fleet_resources (last_snapshot_success_at) WHERE durability = 'durable' AND protection_cadence_seconds IS NOT NULL AND decommissioned_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- (3) the workers' liveness facts
-- ─────────────────────────────────────────────────────────────────────────────
--
-- One row per (component, scope), upserted at the start of every pass. The accept
-- gate reads THESE rows and never configuration.
--
--   offsite — a PLATFORM fact, scope '': every artifact this resource produces
--             reaches the off-provider durability store of record (R2, D-213).
--             D-212 is its SOLE writer; D-202 ships none, so nothing beats it
--             today and every non-waived durable provision refuses naming
--             `offsite-durability-store`. That refusal is the truth of the fleet,
--             and it opens with zero code change the day D-212 beats (J1).
--   cadence — a PER-HOST fact, scope = the fleet host's UUID: the snapshot-cadence
--             worker on THAT host completed the start of a pass.
--
-- ⚠ THE SCOPE-SHAPE CHECK IS LOAD-BEARING (J2). It forbids a global cadence row
-- (one host's beat greening every host's resources — the cross-host false-positive
-- class) and forbids scoped offsite rows (a per-host beat impersonating a platform
-- capability). scope is TEXT and carries no FK: a droppable liveness fact needs no
-- referential integrity, and an FK could not cover the '' platform row at all.
CREATE TABLE fleet_protection_heartbeats (
    component  TEXT        NOT NULL,
    scope      TEXT        NOT NULL DEFAULT '',
    beaten_at  TIMESTAMPTZ NOT NULL,
    detail     TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (component, scope),
    CONSTRAINT fleet_protection_heartbeats_component_ck
        CHECK (component IN ('offsite', 'cadence')),
    CONSTRAINT fleet_protection_heartbeats_scope_shape_ck
        CHECK (
            (component = 'offsite' AND scope =  '')
         OR (component = 'cadence' AND scope <> '')
        )
);

COMMIT;
