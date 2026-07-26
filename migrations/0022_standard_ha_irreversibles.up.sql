-- SentiaeDB `standard-ha` slice 0 (D-196) — the columns and tables that are free
-- today and a migration-under-live-tenant-data later.
--
-- ⚠ WHY NOW. Every shape below is unbackfillable, not merely inconvenient
-- (design §7.1). A host's failure domain is a HUMAN-SUPPLIED fact about a
-- building, a breaker and a switch: once hosts exist and nobody wrote it down at
-- registration, it is permanently unknowable. `availability_class` as its own
-- column is what keeps the tier from being inferred from `tier` (isolation) or
-- `durability` (retention). A role-bearing member SET replaces a scalar
-- `fleet_resources.app_id`, and converting the scalar later is a sweep of every
-- reader plus a data migration. And an RTO computed from discarded stdout is not
-- a published number, so `failover_events` must be rows before the first drill.
--
-- Nothing in this migration has a consumer yet, ON PURPOSE. Slice 1 (the refusal)
-- is the only behaviour this schema is asked to support today: with ONE physical
-- machine the fleet has ONE failure domain, so `standard-ha` is genuinely
-- unprovisionable and the refusal is the whole deliverable.

-- ─────────────────────────────────────────────────────────────────────────────
-- fleet_hosts.failure_domain — the single fact separating HA from theatre
-- ─────────────────────────────────────────────────────────────────────────────
--
-- ENCODING (frozen — see domain.FailureDomain): three POSITIONAL, non-empty,
-- slash-separated segments,
--
--     site/power/network        e.g.  rgalileo-room/breaker-a/switch-1
--
-- each matching ^[a-z0-9][a-z0-9-]*$. Positional and fixed-arity on purpose:
--
--   * A bare label ("host-a", "rack-2") is the fail-open this column exists to
--     close. Three machines in one room on one breaker are three hosts and ONE
--     power domain, and a bare label cannot say so. The first honest answer to
--     "what does this label mean" must not arrive during an incident.
--   * Exactly three segments means an operator CANNOT partially specify it. A
--     keyed form (site=x/power=y) would let `site=x` alone parse, which is the
--     same fail-open in a longer costume.
--   * The comparison the scheduler performs is EQUALITY of the whole string
--     (design §5.1: "different failure_domain"). Segment-wise reasoning ("same
--     site, different power ⇒ good enough") is a policy this platform has not
--     decided, so the encoding records the facts and the placement rule stays
--     the strict one until someone decides otherwise.
--
-- NO DEFAULT in the final state, and usecase.FleetHostRegistry.RegisterHost
-- REFUSES a host that supplies none: a default would be a fail-open, this
-- program's named disease (design §5.1).
--
-- ⚠ THE EXISTING ROW, reasoned from the LIVE DATA, not from convenience. There
-- are exactly two control planes: `runtime_service` on 10.0.10.20 (0 rows in
-- fleet_hosts) and `runtime_service_fc` on the fleet host (EXACTLY ONE row —
-- f1915bca-8c97-5816-a0d5-4e57afecf393, region 'homelab', endpoint
-- 10.0.10.244:50061, registered 2026-07-07). A bare `ADD COLUMN NOT NULL` with no
-- default cannot apply to that row at all, so the choice is a config backfill or
-- a sentinel.
--
-- CONFIG BACKFILL IS REJECTED: this file cannot read the host's env, so a
-- backfill here would have to hardcode a domain triple — inventing, in SQL, the
-- one fact the column exists to force a human to state. It would also read as
-- attested forever.
--
-- SO: the column is added with the transient default 'unattested', which is then
-- DROPPED, leaving a NOT-NULL-no-default column exactly as specified. 'unattested'
-- is deliberately NOT a valid failure domain under the encoding above (it has one
-- segment, not three), which makes it fail CLOSED by construction: the domain
-- parser refuses it, so the host is ineligible for HA placement, and two
-- unattested hosts are NOT two domains. It reads unambiguously as "this row
-- predates failure-domain attestation" — the same precedent as 0019's
-- consistency='unknown' (treat as the weakest class) and 0021's endpoint_id NULL
-- (predates endpoint identity). RegisterHost corrects it the first time the host
-- self-registers WITH a domain configured, and refuses (leaving the sentinel) when
-- it is not.
--
-- Constant default ⇒ catalog-only, no table rewrite (PG11+), squawk-safe.
ALTER TABLE fleet_hosts ADD COLUMN failure_domain TEXT NOT NULL DEFAULT 'unattested';
ALTER TABLE fleet_hosts ALTER COLUMN failure_domain DROP DEFAULT;

-- Absence must never be ''. NOT NULL alone permits the empty string, which is
-- indistinguishable from "not supplied" everywhere except in code that remembers
-- to check — i.e. it is the same fail-open as a default. The SHAPE is validated in
-- the domain (which must also accept the 'unattested' legacy value); this CHECK is
-- only the fence that no writer can store nothing.
ALTER TABLE fleet_hosts
    ADD CONSTRAINT fleet_hosts_failure_domain_ck CHECK (failure_domain <> '');

-- ─────────────────────────────────────────────────────────────────────────────
-- fleet_hosts.region — the OTHER half of the placement invariant (D-196 am. 2)
-- ─────────────────────────────────────────────────────────────────────────────
--
-- The column already exists NOT NULL with no default (migration 0001), so the
-- amendment's schema requirement is met — but only against NULL, not against ''.
-- The invariant is *different failure domain AND SAME REGION*, because D-190 bakes
-- the region into the customer's permanent hostname: a standby in another region
-- would serve a name that names the wrong place. An empty region compares EQUAL to
-- another empty region, so two unlabelled hosts would satisfy "same region"
-- vacuously — the fail-open. Refused here, and RegisterHost refuses it too.
-- The one live row carries 'homelab', so this constraint validates clean.
ALTER TABLE fleet_hosts
    ADD CONSTRAINT fleet_hosts_region_ck CHECK (region <> '');

-- ─────────────────────────────────────────────────────────────────────────────
-- fleet_resources — availability and degrade policy, as their OWN axes
-- ─────────────────────────────────────────────────────────────────────────────
--
-- A THIRD axis, deliberately not folded into an existing one (design §7 slice 0):
--   tier        = ISOLATION  (shared | dedicated)
--   durability  = RETENTION
--   availability_class = whether a second, synchronously-replicating member exists
-- Overloading either of the first two would make one column answer two questions,
-- and the answer a customer is sold ("HA") would then be inferred rather than
-- recorded.
--
-- ⚠ 'ha' HERE MEANS CLAIMED, NOT HELD. Slice 0 is schema and slice 1 is the
-- refusal; members, leases, replication and promotion are slices 2-3. Nothing may
-- read this column as evidence that a standby exists — that evidence is a
-- streaming member row, and it is unbuilt.
ALTER TABLE fleet_resources
    ADD COLUMN availability_class TEXT NOT NULL DEFAULT 'single';
ALTER TABLE fleet_resources
    ADD CONSTRAINT fleet_resources_availability_class_ck
    CHECK (availability_class IN ('single', 'ha'));

-- What a resource does when its synchronous standby is GONE. 'fail_closed' (the
-- default, and the only value the tier is designed around) means the primary
-- cannot acknowledge a commit it did not replicate — the durability promise and
-- the zombie-primary fence are the same mechanism (design §3.4 fence 1).
-- 'fail_open' trades RPO for availability and exists so the escape hatch is an
-- explicit, per-resource, auditable ROW rather than an operator's ad-hoc edit to a
-- config file.
ALTER TABLE fleet_resources
    ADD COLUMN sync_degrade_policy TEXT NOT NULL DEFAULT 'fail_closed';
ALTER TABLE fleet_resources
    ADD CONSTRAINT fleet_resources_sync_degrade_policy_ck
    CHECK (sync_degrade_policy IN ('fail_closed', 'fail_open'));

-- ─────────────────────────────────────────────────────────────────────────────
-- fleet_resource_members (design §5.3) — the standby cannot be a second replica
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Three existing guards structurally forbid a second replica of the same app
-- (MinReplicas/MaxReplicas: 1, ErrVolumeAppNotScalable, and fleet_volumes UNIQUE
-- (app_id, mount_path) with a single attached_replica) — and all three are
-- LOAD-BEARING: two replicas of one app would attach the SAME backing file. So a
-- member is its own fleet_app with its own volume, and the resource is the set.
--
-- This supersedes the scalar fleet_resources.app_id as the source of truth. The
-- scalar stays as a denormalized pointer for one release and is dropped under the
-- 2-deploy rule (§24) — not in this migration.
--
-- ⚠ FKs are ON DELETE RESTRICT, never SET NULL: a member whose parent resource or
-- host silently vanished is an allocation nobody owns — a running VM holding a
-- customer's data with no claim above it. Refusing the parent delete is the honest
-- outcome; a NULLed pointer is a leak that looks like a clean row.
--
-- app_id carries NO FK on purpose: it points at fleet_apps, whose lifecycle is
-- owned by the workload plane and whose rows are torn down by paths that must not
-- be blocked by a member row. A member for a vanished app is detectable
-- (conditionBackingAppMissing already names that class); a member for a vanished
-- RESOURCE is not.
CREATE TABLE fleet_resource_members (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id  UUID        NOT NULL REFERENCES fleet_resources(id) ON DELETE RESTRICT,
    app_id       UUID        NOT NULL,
    role         TEXT        NOT NULL CHECK (role IN ('primary', 'standby')),
    -- generation is the resource incarnation this member belongs to. It is the
    -- fence that keeps a revived old member from satisfying a new timeline's
    -- quorum (design §8 row 6) and it is why the application_name is
    -- generation-scoped.
    generation   INT         NOT NULL CHECK (generation >= 1),
    state        TEXT        NOT NULL CHECK (state IN ('seeding', 'streaming', 'catching_up', 'promoted', 'retired')),
    host_id      UUID        REFERENCES fleet_hosts(id) ON DELETE RESTRICT,
    promoted_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Exactly one primary per resource, enforced by the DATABASE and not by code.
-- This is the split-brain fence at the membership layer: PostgreSQL is
-- single-writer, so two concurrent promotions cannot both land a primary row
-- however racy the callers are. A code-level check would be a read-then-write with
-- a window in it.
CREATE UNIQUE INDEX fleet_resource_members_one_primary
    ON fleet_resource_members (resource_id)
    WHERE role = 'primary' AND state <> 'retired';

CREATE INDEX fleet_resource_members_resource_idx ON fleet_resource_members (resource_id);
CREATE INDEX fleet_resource_members_host_idx     ON fleet_resource_members (host_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- fleet_resource_leases (design §3.2) — the only authority on who is primary
-- ─────────────────────────────────────────────────────────────────────────────
--
-- ⚠ CLOCK AUTHORITY (frozen, D-196 amendment 4). EVERY lease and promotion time
-- comparison is evaluated by THIS database, using ITS now(). No host clock, no
-- application clock, and no timestamp computed in Go is ever compared against
-- these columns. Renewal is
--     UPDATE ... SET expires_at = now() + interval '15 seconds', renewed_at = now()
--     WHERE resource_id=$r AND generation=$g AND holder_host_id=$h
-- and promotion is
--     UPDATE ... SET generation = generation+1, holder_host_id=$s, holder_member_id=$m,
--                    expires_at = now() + interval '15 seconds'
--     WHERE resource_id=$r AND generation=$g AND expires_at < now() - interval '5 seconds'
-- — min_promotion_delay lives IN THE PREDICATE, so no retry path can skip it, and
-- because Postgres is single-writer at most one promotion can win globally per
-- generation. A future refactor that passes a host-computed time.Now() into either
-- statement re-introduces a distributed-clock bug into the one place this design
-- has none; that is why this paragraph is in the schema and not only in a design
-- document.
--
-- resource_id is the PRIMARY KEY, not (resource_id, generation): there is one
-- lease per resource and the generation is a COLUMN that advances by CAS. A
-- history table would make "who holds it now" a query with an ordering in it.
-- Keying on the RESOURCE rather than on a member pair is also what makes ha-3
-- (ANY 1 of 2 standbys) a member count instead of a redesign.
CREATE TABLE fleet_resource_leases (
    resource_id      UUID        PRIMARY KEY REFERENCES fleet_resources(id) ON DELETE RESTRICT,
    generation       INT         NOT NULL CHECK (generation >= 1),
    holder_host_id   UUID        NOT NULL REFERENCES fleet_hosts(id) ON DELETE RESTRICT,
    holder_member_id UUID        NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    renewed_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- failover_events — the rows the published RTO is computed FROM
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Drills and real failovers live in ONE table so the published number cannot be
-- computed from a friendlier population than the real one (design §7 slice 0).
--
-- ⚠ THE CAUSE TAXONOMY IS UNBACKFILLABLE (D-196 amendment 4). Nothing about a
-- finished row reveals whether it was a real failure, a scheduled drill, or a
-- planned switchover, and those three have materially different durations: a
-- switchover has no detection delay at all, so a population that mixes them
-- reports an RTO nobody experienced. A table without this column can NEVER be
-- re-labelled — the knowledge is gone the moment the event ends.
--
--   real        — an unplanned failure the detector observed
--   drill       — a deliberate exercise of the path (the trust surface's source)
--   switchover  — a PLANNED move (cordon/drain): quiesce, full catch-up, promote
CREATE TABLE failover_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id     UUID NOT NULL REFERENCES fleet_resources(id) ON DELETE RESTRICT,
    cause           TEXT NOT NULL CHECK (cause IN ('real', 'drill', 'switchover')),
    -- The TRIGGERING WITNESS SET (design §3.5). Promotion is permitted only on
    -- W1 AND (W2 OR W3); recording which witnesses fired is what makes an
    -- after-the-fact audit of that rule possible, and what distinguishes "the
    -- lease expired and the standby also could not reach the primary" from
    -- "something promoted on a partition alone" — the forbidden trigger.
    witnesses       TEXT[] NOT NULL DEFAULT '{}',
    from_generation INT  NOT NULL CHECK (from_generation >= 1),
    -- The new incarnation. Strictly greater: a promotion that did not advance the
    -- generation did not fence anything.
    to_generation   INT  NOT NULL CHECK (to_generation > from_generation),
    -- The members involved. No FK: a retired member row may be reaped while its
    -- failover history must survive — this table is the evidence, not a pointer.
    from_member_id  UUID,
    to_member_id    UUID,
    outcome         TEXT NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'abandoned')),
    -- ⚠ ALL THREE TIMESTAMPS ARE CONTROL-PLANE-DB TIMES (see the clock-authority
    -- note above). rto_seconds is client_writable_at - detected_at, so a
    -- host-clock-derived value would publish a number measured on a clock nobody
    -- audits — and skew would make it flattering as easily as pessimistic.
    detected_at        TIMESTAMPTZ NOT NULL,
    -- When the CAS won. NULL on an outcome that never promoted.
    promoted_at        TIMESTAMPTZ,
    -- When a client could WRITE again — the end of the interval the customer
    -- actually experienced, and the only honest terminus for RTO. A promotion is
    -- not a recovery until the connection path follows it.
    client_writable_at TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A witness vocabulary fence, so the taxonomy cannot rot into free text.
    -- Matches domain.FailoverWitness*.
    CONSTRAINT failover_events_witnesses_ck CHECK (
        witnesses <@ ARRAY[
            'w1_lease_expired',
            'w2_standby_replication_lost',
            'w3_gate_backend_unreachable'
        ]::TEXT[]
    ),
    -- A REAL failover with no recorded witness is unattributable: it says a
    -- promotion happened but not that the rule permitting it was satisfied. Drills
    -- and switchovers are triggered by an operator, not by a witness, so they
    -- legitimately carry none.
    CONSTRAINT failover_events_real_needs_witness_ck CHECK (
        cause <> 'real' OR COALESCE(array_length(witnesses, 1), 0) >= 1
    ),
    -- RTO is only computable over a completed interval, and it must never be
    -- negative — a row that claims a client was writable before the failure was
    -- detected is a clock or a wiring bug, and it would drag a published average
    -- down silently.
    CONSTRAINT failover_events_interval_ck CHECK (
        client_writable_at IS NULL OR client_writable_at >= detected_at
    )
);

CREATE INDEX failover_events_resource_idx ON failover_events (resource_id, detected_at DESC);
CREATE INDEX failover_events_cause_idx    ON failover_events (cause, detected_at DESC);
