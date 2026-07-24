-- runtime-fleet CP4.5 §9 #3 (D-164/D-183) — the P19 resource control-plane
-- foundation. fleet_resources is the durable claim ledger (idempotency anchored
-- on (owner_org, claim_key, env)); fleet_resource_recovery_points is the
-- snapshot/backup catalog that SURVIVES a resource tombstone (no cascade delete),
-- so a decommissioned resource can still be restored from.
--
-- No RLS: this matches every fleet control-plane table (0001) — owner_org is a
-- data column and the carriage checks are the gate, not row-level security.

CREATE TABLE fleet_resources (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_org         UUID         NOT NULL,
    claim_key         TEXT         NOT NULL,
    env               TEXT         NOT NULL,
    revision          INT          NOT NULL DEFAULT 1,
    class             TEXT         NOT NULL,
    tier              TEXT         NOT NULL,
    phase             TEXT         NOT NULL,
    app_id            UUID,
    db_name           TEXT         NOT NULL DEFAULT '',
    role_name         TEXT         NOT NULL DEFAULT '',
    endpoint          TEXT         NOT NULL DEFAULT '',
    secret_refs       TEXT[]       NOT NULL DEFAULT '{}',
    system_id         TEXT         NOT NULL DEFAULT '',
    params            JSONB,
    expires_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decommissioned_at TIMESTAMPTZ,
    UNIQUE (owner_org, claim_key, env)
);
CREATE INDEX fleet_resources_app_id_idx ON fleet_resources (app_id);

CREATE TABLE fleet_resource_recovery_points (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID        NOT NULL REFERENCES fleet_resources(id),
    volume_id   UUID,
    object_key  TEXT        NOT NULL,
    kind        TEXT        NOT NULL,
    size_bytes  BIGINT,
    verified    BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX fleet_resource_recovery_points_resource_idx
    ON fleet_resource_recovery_points (resource_id, created_at);
