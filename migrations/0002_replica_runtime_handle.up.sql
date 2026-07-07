-- runtime-fleet CP4 §9#6 — resident supervised replica runtime handle.
-- fleet_replicas gained a durable Firecracker teardown handle so a resident
-- replica is decommissionable exactly like an image workload. All columns are
-- NOT NULL DEFAULT so existing rows stay valid (squawk-safe additive DDL).

ALTER TABLE fleet_replicas ADD COLUMN rootfs_path VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE fleet_replicas ADD COLUMN socket_path VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE fleet_replicas ADD COLUMN tap_name    VARCHAR(32)  NOT NULL DEFAULT '';
ALTER TABLE fleet_replicas ADD COLUMN port        INT          NOT NULL DEFAULT 0;
