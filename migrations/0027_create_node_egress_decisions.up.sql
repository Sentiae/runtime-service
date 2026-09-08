-- D-395 — the egress audit trail survives the sidecar.
--
-- One row per (invocation, decision, reason, host, port), written by the runtime
-- from the sidecar's OWN log immediately before the sidecar is removed
-- (SidecarManager.drainAudit) on every removal path: completion, failure,
-- cancellation, the run sweep, the boot sweep and the orphan sweep.
-- request_count is how many requests hit the tuple; capped marks rows whose
-- sidecar stopped writing decision lines at its per-invocation cap
-- (request_count is then a floor).
--
-- Tenancy: the RUN owns the row. run_id references graph_executions, which
-- carries organization_id; there is deliberately no organization_id here — a row
-- cannot exist without a run and a run cannot exist without an org, so every read
-- is scoped by joining the run, exactly as node_executions are read today.
-- ON DELETE CASCADE is the retention rule and its check: the audit lives as long
-- as its run.
--
-- Contents: decision and reason are CLOSED vocabularies, CHECKed here rather
-- than only in the sidecar that writes them; host is the normalized name the
-- node asked for — tenant-chosen, bounded to DNS length — or the literal
-- '[redacted]' when that name carried one of the invocation's own bound
-- secrets, and host_redacted is true in exactly that case (a CHECK, so the flag
-- cannot disagree with the string). The proxy token, the handles and every
-- secret value are never in the sidecar's decision and never reach this table.
--
-- port is bigint, not integer: squawk's prefer-bigint-over-int is a hard rule of
-- the migration gate and the only ignore this file is permitted is the foreign
-- key one. The CHECK is what actually bounds a port to 1..65535.
--
-- Numbering: 0027 is next-free (0026's note on m.Up() and skipped versions).
-- New table: no rewrite, no lock held on existing rows. One explicit transaction.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

CREATE TABLE node_egress_decisions (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        uuid        NOT NULL REFERENCES graph_executions (id) ON DELETE CASCADE,
    invocation_id text        NOT NULL,
    node          text        NOT NULL,
    decision      text        NOT NULL,
    reason        text        NOT NULL,
    host          text        NOT NULL,
    host_redacted boolean     NOT NULL DEFAULT false,
    port          bigint      NOT NULL,
    request_count bigint      NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    capped        boolean     NOT NULL DEFAULT false,
    recorded_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT node_egress_decisions_decision_ck CHECK (decision IN ('allow', 'deny')),
    CONSTRAINT node_egress_decisions_reason_ck   CHECK (
        (decision = 'allow' AND reason IN ('manifest_wildcard', 'manifest_exact', 'manifest_suffix'))
        OR
        (decision = 'deny'  AND reason IN ('token_missing', 'policy_missing', 'private_address',
                                           'ip_literal', 'port_not_allowed', 'host_not_declared',
                                           'resolve_failed', 'dns_no_answer', 'own_address'))
    ),
    CONSTRAINT node_egress_decisions_port_ck     CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT node_egress_decisions_count_ck    CHECK (request_count > 0),
    CONSTRAINT node_egress_decisions_host_ck     CHECK (length(host) BETWEEN 1 AND 253),
    CONSTRAINT node_egress_decisions_redacted_ck CHECK (host_redacted = (host = '[redacted]')),
    CONSTRAINT node_egress_decisions_tuple_uq    UNIQUE (invocation_id, decision, reason, host, port)
);

CREATE INDEX node_egress_decisions_run_idx ON node_egress_decisions (run_id);

COMMIT;
