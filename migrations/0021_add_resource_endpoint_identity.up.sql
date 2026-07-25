-- SentiaeDB Phase 0 (D-190) — a customer database endpoint becomes a PERMANENT,
-- opaque, region-scoped identity carried on the resource row.
--
-- ⚠ WHY NOW, AND WHY IT CANNOT WAIT. A database's hostname is the most permanent
-- artifact this platform hands a customer: the instant it is pasted into an
-- application's config, changing it breaks that application. Today
-- delivery-service has ZERO InfraProvisioner consumers, so nobody holds a
-- connection string — which makes this migration free. After the first one is
-- handed out it is a migration under live connection strings, which is not a
-- migration at all. db-gate does not exist yet ON PURPOSE: a resource created
-- before the gate must already carry the name the gate will later serve.
--
-- The name is  <endpoint_id>.<region>.<zone>  e.g.
--   quiet-forest-4821.eu-central.db.sentiae.com
--
--   endpoint_id  readable random (adjective-noun-NNNN), minted from crypto/rand
--                at resource birth, IMMUTABLE for life. Derived from NOTHING —
--                not the claim key, the org, the app, the host or the resource
--                uuid — so a claim can be renamed and every internal key (object
--                prefix, lease, uid) rotated without touching a connection
--                string a customer already holds. Nothing about the TENANT is
--                encoded: SNI and DNS resolver logs are semi-public, and
--                `acme-corp.db.sentiae.com` would publish the customer list.
--   region       stamped at birth from config, never inferred per request. Data
--                never silently crosses a region, so encoding it removes a global
--                routing tier from the connect path at the cost of one wildcard
--                certificate per region.
--   zone         config only; it is NOT stored, because it is the same for every
--                resource a deployment serves and storing it would create a
--                second, divergent source for one name.
--
-- ⚠ NULLABLE, and the unique index tolerates that: Postgres does not collide
-- NULLs, so every pre-existing row and every endpoint-less (shared-tier) claim
-- coexists under one unique index. Absence is NULL and never '' — two '' rows
-- would be a spurious conflict.
--
-- BACKFILL: NONE, deliberately. Reasoning from the live data, not from
-- convenience — the only control planes that exist are runtime_service (0 rows
-- in fleet_resources) and runtime_service_fc on the fleet host (71 rows, 68 of
-- them already tombstoned; the 3 live ones are the `canarybig` / `xorg-*`
-- verification claims of the P19 drills). There is not one customer resource in
-- existence, which is the entire reason this lands today. Beyond that, a SQL
-- backfill would have to re-implement the curated word lists AND an entropy
-- source inside this file — a second, divergent minter for the one artifact that
-- must have exactly one, and one whose randomness nobody would ever audit. So:
-- pre-existing rows keep endpoint_id NULL, which reads unambiguously as "this
-- resource predates endpoint identity". It is not a half-populated column by
-- accident: every resource born from here on mints one in the same INSERT that
-- creates it, and a resource with NULL is one db-gate must refuse to serve
-- rather than guess a name for.
--
-- Additive columns with CONSTANT defaults (PG11+ stores them in the catalog) →
-- no table rewrite, no lock beyond the catalog update (squawk-safe). Plain
-- (non-CONCURRENT) index: fleet_resources is tiny, and keeping the whole change
-- in one transaction matters more than the brief lock — a half-applied endpoint
-- identity is exactly the ambiguity this migration exists to remove.

ALTER TABLE fleet_resources ADD COLUMN endpoint_id TEXT;
ALTER TABLE fleet_resources ADD COLUMN region      TEXT NOT NULL DEFAULT '';

-- generation rides along DELIBERATELY, ahead of its first consumer. Archive and
-- recovery-point object prefixes are generation-scoped in the ratified
-- durability sequencing, so a restore or a rebuild opens a NEW prefix instead of
-- interleaving artifacts with the incarnation it replaced. Adding the column
-- after archiving has begun is a repository-LAYOUT migration under live tenant
-- data; adding it now is one DDL statement against rows nobody owns.
ALTER TABLE fleet_resources ADD COLUMN generation  INT  NOT NULL DEFAULT 1;

-- Generation 0 is not a generation. The CHECK exists because GORM writes every
-- field of a struct it saves: a creation path that forgets to stamp the initial
-- generation would otherwise silently insert 0 and produce a generation-0 object
-- prefix that no reader expects. This turns that into a refused INSERT.
ALTER TABLE fleet_resources
    ADD CONSTRAINT fleet_resources_generation_ck CHECK (generation >= 1);

-- The SHAPE fence, not the vocabulary one. The word lists live in the domain
-- (internal/domain/resource_endpoint_words.go) and may only grow, so the store
-- must never encode them — but a value that is not <lowercase>-<lowercase>-<4
-- digits> is not an endpoint id under any version of those lists, and letting one
-- in means storing a permanent name the gate cannot serve.
ALTER TABLE fleet_resources
    ADD CONSTRAINT fleet_resources_endpoint_id_ck
    CHECK (endpoint_id IS NULL OR endpoint_id ~ '^[a-z]+-[a-z]+-[0-9]{4}$');

-- The arbiter. Uniqueness of a customer-facing name is decided HERE and nowhere
-- else: the minter draws from ~4×10^8 combinations, which is a low collision
-- probability, never a guarantee. The provision path re-mints on a violation of
-- this index, bounded.
CREATE UNIQUE INDEX fleet_resources_endpoint_id_key ON fleet_resources (endpoint_id);
