-- CP4.5 §9 #5 — P21 fleet network fabric (I8: no console-only state).
-- A fleet network is a per-(system_id, env) POLICY SCOPE, not a CIDR: the fleet
-- addresses host-globally (10.201.0.0/16, one /30 per replica boot), so there is
-- no per-system CIDR to store (D-164). system_id is an opaque scope key (the
-- catalog Product ID) the fleet compares and never dereferences.
CREATE TABLE IF NOT EXISTS fleet_networks (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    system_id  VARCHAR(255) NOT NULL,
    env        VARCHAR(64)  NOT NULL,
    owner_org  TEXT         NOT NULL DEFAULT '',
    status     VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_fleet_networks_system_env UNIQUE (system_id, env)
);
CREATE INDEX IF NOT EXISTS fleet_networks_status_idx ON fleet_networks (status);

-- One compiled arch edge. Keyed on COMPONENT IDs, never IPs: a replica's guest
-- IP is per-boot, so IPs are re-resolved from live replicas each reconcile tick.
CREATE TABLE IF NOT EXISTS fleet_network_policies (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    network_id           UUID         NOT NULL REFERENCES fleet_networks(id) ON DELETE CASCADE,
    from_component_id    VARCHAR(255) NOT NULL,
    to_component_id      VARCHAR(255) NOT NULL,
    protocol             VARCHAR(10)  NOT NULL,
    port                 INT          NOT NULL,
    derived_from_edge_id VARCHAR(255) NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- The DB half of the same fail-closed property as FleetNetworkPolicy.Validate:
    -- port = 0 cannot exist, so it can never be read back and reinterpreted as a
    -- wildcard allow.
    CONSTRAINT fleet_network_policies_port_ck CHECK (port > 0 AND port <= 65535)
);
CREATE INDEX IF NOT EXISTS fleet_network_policies_network_id_idx ON fleet_network_policies (network_id);
