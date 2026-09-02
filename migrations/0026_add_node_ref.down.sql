-- Reverses 0026. One explicit transaction mirroring the up, dropping in
-- reverse order: either the v25 shape is fully restored or nothing changes.
--
-- Nothing here refuses. Every column 0026 adds is DERIVABLE again — a graph's
-- pins, ports, role, secrets and egress all come from the compiled execution
-- plan the deployer hands CreateGraph, so a rolled-back schema is re-filled by
-- the next compile rather than reconstructed from a backup. That is why this
-- rollback is unconditional where 0024's and 0025's are not: those drop audit
-- records with no other copy, and this one drops a projection of an artifact.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

ALTER TABLE node_executions DROP COLUMN IF EXISTS node_ref;

ALTER TABLE graph_nodes DROP CONSTRAINT IF EXISTS graph_nodes_role_ck;

ALTER TABLE graph_nodes
    DROP COLUMN IF EXISTS egress,
    DROP COLUMN IF EXISTS secrets,
    DROP COLUMN IF EXISTS role,
    DROP COLUMN IF EXISTS ports,
    DROP COLUMN IF EXISTS node_ref;

COMMIT;
