-- Phase 4 — a graph node IS a built bundle, so the row must carry the pin.
--
-- Five columns on graph_nodes and one on node_executions. They are what the
-- interpreter's node_type/language/code become once a node is a published,
-- digest-pinned artifact rather than a snippet the runtime interprets:
--
--   node_ref  — the resolved pin: qualified name, semver, language, image ref
--               and the sha256 DIGEST the sandbox runs by. Nullable because
--               the 13 pre-Phase-4 rows have no pin and can never be given
--               one; ExecuteGraph refuses such a graph (ErrLegacyGraph) rather
--               than inventing a bundle for it (T-RUN-LEGACY-GRAPH-ROWS-DROP
--               removes them later).
--   ports     — the manifest's declared inputs/outputs with their required
--               flags, so the runtime can rebuild the execution plan from the
--               rows alone and never has to re-read a manifest at run time.
--   role      — "" | trigger | respond. NOT NULL DEFAULT '' with a CHECK:
--               the vocabulary is three values and a fourth is a graph the
--               runtime cannot execute, so it is unrepresentable here rather
--               than merely refused in Go.
--   secrets   — the declared secret specs (name + required). Values NEVER
--               live here: the runtime resolves them per invocation and hands
--               them to a sidecar on an attached stdin stream (D-4).
--   egress    — the manifest's egress allowlist patterns. Empty/absent means
--               the sandbox runs with --network none.
--
--   node_executions.node_ref — the pin a row ACTUALLY ran, recorded per
--               execution so a completed run stays attributable after the
--               graph is re-pinned.
--
-- Numbering: 0026 is next-free. RunMigrations is golang-migrate m.Up() over
-- the embedded FS, which applies only versions GREATER than the recorded one
-- and performs NO missing-migration detection, so a higher number landing
-- first would permanently skip a lower one (0025's J2 note).
--
-- Locking + squawk: graph_nodes and node_executions are small (13 legacy
-- graph_nodes rows live today) and have one writer, so the whole change runs
-- as ONE explicit transaction — either the Phase 4 shape lands or nothing
-- does. lock_timeout bounds every lock wait so a stuck autovacuum makes this
-- fail fast rather than queue behind it. Every added column is NULLABLE or
-- has a constant DEFAULT (catalog-only, no rewrite on PG11+), so no lock is
-- held for a table scan. The one squawk-ignore is line-scoped: the two-phase
-- NOT VALID / VALIDATE dance would cost the atomicity of this file and buys
-- nothing at this row count.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

ALTER TABLE graph_nodes ADD COLUMN node_ref JSONB;
ALTER TABLE graph_nodes ADD COLUMN ports JSONB;
ALTER TABLE graph_nodes ADD COLUMN role TEXT NOT NULL DEFAULT '';
ALTER TABLE graph_nodes ADD COLUMN secrets JSONB;
ALTER TABLE graph_nodes ADD COLUMN egress JSONB;

-- squawk-ignore constraint-missing-not-valid
ALTER TABLE graph_nodes ADD CONSTRAINT graph_nodes_role_ck CHECK (role IN ('', 'trigger', 'respond'));

ALTER TABLE node_executions ADD COLUMN node_ref JSONB;

COMMIT;
