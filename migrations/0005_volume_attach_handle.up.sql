-- runtime-fleet CP4 rt#9 — persistent volume attach + backing-file handle.
-- fleet_volumes gains the durable state to materialize an ext4 backing file, pin
-- it to a resident replica (single-writer), and mount it in the guest as a 2nd
-- virtio-blk device. All columns are NOT NULL DEFAULT (or nullable) so existing
-- rows stay valid (squawk-safe additive DDL).

ALTER TABLE fleet_volumes ADD COLUMN backing_path VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE fleet_volumes ADD COLUMN attached_replica UUID;
ALTER TABLE fleet_volumes ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'available';
ALTER TABLE fleet_volumes ADD COLUMN device_name VARCHAR(16) NOT NULL DEFAULT '/dev/vdb';
