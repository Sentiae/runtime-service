-- runtime-fleet CP4 — durable fleet control plane (I8: no console-only state).
-- Host registry, resident-replica desired/actual, placements, routes, volumes,
-- secret bindings. golang-migrate owns this DDL; these tables are NOT in the
-- AutoMigrate list.

CREATE TABLE fleet_hosts (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    region              VARCHAR(64)  NOT NULL,
    labels              JSONB        NOT NULL DEFAULT '{}',
    capacity_vcpu       INT          NOT NULL,
    capacity_mem_mb     BIGINT       NOT NULL,
    capacity_disk_mb    BIGINT       NOT NULL,
    allocatable_vcpu    INT          NOT NULL,
    allocatable_mem_mb  BIGINT       NOT NULL,
    allocatable_disk_mb BIGINT       NOT NULL,
    health              VARCHAR(20)  NOT NULL DEFAULT 'unknown',
    status              VARCHAR(20)  NOT NULL DEFAULT 'active',
    endpoint            VARCHAR(255) NOT NULL DEFAULT '',
    last_heartbeat      TIMESTAMPTZ,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_hosts_status_idx ON fleet_hosts (status);
CREATE INDEX fleet_hosts_health_idx ON fleet_hosts (health);

CREATE TABLE fleet_apps (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    component_id     VARCHAR(255) NOT NULL,
    env              VARCHAR(64)  NOT NULL,
    image_repository VARCHAR(512) NOT NULL,
    image_digest     VARCHAR(255) NOT NULL,
    desired_replicas INT          NOT NULL DEFAULT 1,
    min_replicas     INT          NOT NULL DEFAULT 0,
    max_replicas     INT          NOT NULL DEFAULT 1,
    scale_to_zero    BOOLEAN      NOT NULL DEFAULT false,
    port             INT          NOT NULL DEFAULT 0,
    resources_vcpu   INT          NOT NULL DEFAULT 1,
    resources_mem_mb BIGINT       NOT NULL DEFAULT 512,
    restart_policy   VARCHAR(20)  NOT NULL DEFAULT 'always',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (component_id, env)
);
CREATE INDEX fleet_apps_component_id_idx ON fleet_apps (component_id);

CREATE TABLE fleet_replicas (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id           UUID         NOT NULL REFERENCES fleet_apps(id) ON DELETE CASCADE,
    host_id          UUID         REFERENCES fleet_hosts(id) ON DELETE SET NULL,
    image_repository VARCHAR(512) NOT NULL DEFAULT '',
    image_digest     VARCHAR(255) NOT NULL DEFAULT '',
    state            VARCHAR(20)  NOT NULL DEFAULT 'scheduled',
    endpoint         VARCHAR(512) NOT NULL DEFAULT '',
    guest_ip         VARCHAR(45)  NOT NULL DEFAULT '',
    host_port        INT          NOT NULL DEFAULT 0,
    net_index        INT          NOT NULL DEFAULT 0,
    pid              INT,
    exit_code        INT,
    restart_policy   VARCHAR(20)  NOT NULL DEFAULT 'always',
    message          TEXT         NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_replicas_app_id_idx ON fleet_replicas (app_id);
CREATE INDEX fleet_replicas_host_id_idx ON fleet_replicas (host_id);
CREATE INDEX fleet_replicas_state_idx ON fleet_replicas (state);

CREATE TABLE fleet_placements (
    replica_id      UUID PRIMARY KEY REFERENCES fleet_replicas(id) ON DELETE CASCADE,
    host_id         UUID        NOT NULL REFERENCES fleet_hosts(id) ON DELETE CASCADE,
    constraint_type VARCHAR(20) NOT NULL DEFAULT 'bin_pack',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE fleet_routes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id        UUID         NOT NULL REFERENCES fleet_apps(id) ON DELETE CASCADE,
    host_pattern  VARCHAR(255) NOT NULL,
    path_prefix   VARCHAR(255) NOT NULL DEFAULT '/',
    custom_domain VARCHAR(255) NOT NULL DEFAULT '',
    tls_cert_ref  VARCHAR(255) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_routes_app_id_idx ON fleet_routes (app_id);

CREATE TABLE fleet_volumes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id        UUID         NOT NULL REFERENCES fleet_apps(id) ON DELETE CASCADE,
    size_mb       BIGINT       NOT NULL,
    host_affinity UUID         REFERENCES fleet_hosts(id) ON DELETE SET NULL,
    snapshot_ref  VARCHAR(255) NOT NULL DEFAULT '',
    mount_path    VARCHAR(255) NOT NULL DEFAULT '/data',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_volumes_app_id_idx ON fleet_volumes (app_id);

CREATE TABLE fleet_secret_bindings (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id     UUID         NOT NULL REFERENCES fleet_apps(id) ON DELETE CASCADE,
    secret_ref VARCHAR(255) NOT NULL,
    inject_as  VARCHAR(10)  NOT NULL DEFAULT 'env',
    target     VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_secret_bindings_app_id_idx ON fleet_secret_bindings (app_id);
