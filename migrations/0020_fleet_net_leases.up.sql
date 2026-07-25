-- SentiaeDB Phase 0 — the microVM addressing plane becomes a DURABLE ALLOCATION,
-- not a process-local map.
--
-- ⚠ WHY. One integer (the net index) keys the TAP device name, the /30 the guest
-- boots with, the jailer chroot id AND the per-VM uid/gid. Until now it was
-- handed out by an in-memory map inside one process, SEEDED at startup from the
-- net_index columns of live rows. Everything about that is fail-OPEN:
--
--   * the seed swallowed its own errors (a DB blip on boot → an empty used-set →
--     the next boot allocates index 1, which a live customer VM already holds:
--     same uid, same chroot, same address — cross-tenant read/write);
--   * the seed's state filter never covered `dead`, while RefreshHealth marks a
--     replica dead WITHOUT stopping its VMM — so a still-running VM's index was
--     considered free;
--   * two processes (or one restarted mid-boot) share no map at all;
--   * the index space was host-global with no host term, so a second fleet host
--     would allocate the same indices as the first.
--
-- The fix is that the ALLOCATION IS A ROW. A lease exists if and only if the
-- address/uid/tap/chroot is held; the INSERT against a UNIQUE index is the
-- serialization point, so a collision is a refused boot rather than a silent
-- overlap. Nothing here is a partial index over states: fleet_replicas and
-- image_workloads share one index space and their "occupying" state sets move, so
-- state can never be the fence.
--
-- Coordinates (see internal/domain/fleet_net_lease.go — the ONE place they are
-- computed): net_index = net_ordinal*1024 + local_slot, with net_ordinal in
-- [0,15] per host and local_slot in [1,1023] host-local. 16 hosts × 1023 slots
-- covers the whole [1,16383] index space of 10.201.0.0/16 (the last /30 is based
-- at 65532, guest 10.201.255.254). local_slot — not the global index — keys
-- vm_uid, tap name and jail id, which keeps the uid inside the configured per-VM
-- span and the device name short.
--
-- The addresses are RECORDED columns, never recomputed on read. That is what
-- makes a future re-split migration-free: changing the stride can never move an
-- address a live VM is already configured with.
--
-- No RLS: this matches every fleet control-plane table (0001).

-- ── The host term ────────────────────────────────────────────────────────────
-- NULLABLE on purpose, and NULL must never read as 0: ordinal 0 is a real block
-- some host legitimately owns, so a host with no ordinal allocates NOTHING rather
-- than defaulting into another host's addresses.
ALTER TABLE fleet_hosts ADD COLUMN net_ordinal INT;
CREATE UNIQUE INDEX fleet_hosts_net_ordinal_key ON fleet_hosts (net_ordinal);
ALTER TABLE fleet_hosts ADD CONSTRAINT fleet_hosts_net_ordinal_ck
    CHECK (net_ordinal IS NULL OR (net_ordinal >= 0 AND net_ordinal <= 15));

-- ── The lease ────────────────────────────────────────────────────────────────
CREATE TABLE fleet_net_leases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ON DELETE RESTRICT, never SET NULL/CASCADE: a host row may not be deleted
    -- while microVMs on it still hold addresses, and a lease with a NULL host
    -- would be an allocation nobody can reclaim.
    host_id      UUID        NOT NULL REFERENCES fleet_hosts(id) ON DELETE RESTRICT,
    -- Snapshot of the host's net_ordinal AT ALLOCATION TIME, deliberately not a
    -- join: a live VM's addresses must never move because a host row was edited.
    host_ordinal INT         NOT NULL,
    local_slot   INT         NOT NULL,
    net_index    INT         NOT NULL,
    host_ip      VARCHAR(45) NOT NULL,
    guest_ip     VARCHAR(45) NOT NULL,
    tap_name     VARCHAR(15) NOT NULL,
    vm_uid       INT         NOT NULL,
    owner_kind   VARCHAR(16) NOT NULL,
    owner_id     UUID        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fleet_net_leases_owner_kind_ck CHECK (owner_kind IN ('replica','workload')),
    CONSTRAINT fleet_net_leases_slot_ck  CHECK (local_slot > 0),
    CONSTRAINT fleet_net_leases_index_ck  CHECK (net_index > 0 AND net_index <= 16383),
    CONSTRAINT fleet_net_leases_uid_ck    CHECK (vm_uid > 0)
);

-- The five fences. Each one is a distinct way two VMs could collide, so each is
-- enforced separately rather than inferred from the index: net_index is the /30
-- (fleet-global), local_slot the jail id, vm_uid the unprivileged identity,
-- tap_name the host device, and (owner_kind, owner_id) makes an allocation
-- at-most-once per owner so a retry adopts instead of allocating twice.
CREATE UNIQUE INDEX fleet_net_leases_net_index_key ON fleet_net_leases (net_index);
CREATE UNIQUE INDEX fleet_net_leases_host_slot_key ON fleet_net_leases (host_id, local_slot);
CREATE UNIQUE INDEX fleet_net_leases_host_uid_key  ON fleet_net_leases (host_id, vm_uid);
CREATE UNIQUE INDEX fleet_net_leases_host_tap_key  ON fleet_net_leases (host_id, tap_name);
CREATE UNIQUE INDEX fleet_net_leases_owner_key     ON fleet_net_leases (owner_kind, owner_id);
CREATE INDEX fleet_net_leases_host_idx ON fleet_net_leases (host_id);

-- Plain (non-CONCURRENT) index creation: these are empty/tiny tables on a
-- single-host fleet, and keeping the whole migration in one transaction matters
-- more here — a half-applied addressing plane is exactly the fail-open state this
-- migration exists to remove.

-- ── Backfill: host ordinals ──────────────────────────────────────────────────
-- Oldest host first, so the existing host takes ordinal 0 and its live VMs keep
-- the addresses they are already running on. Hosts beyond the 16th get no
-- ordinal, which refuses boots there rather than aliasing another host's block.
WITH ordered AS (
    SELECT id, (row_number() OVER (ORDER BY created_at, id)) - 1 AS ord
    FROM fleet_hosts
)
UPDATE fleet_hosts h
SET net_ordinal = o.ord
FROM ordered o
WHERE h.id = o.id
  AND o.ord <= 15;

-- ── Backfill: leases for live replicas ───────────────────────────────────────
-- Every replica state that OCCUPIES an index, including `dead`: RefreshHealth
-- marks a replica dead without stopping its VMM, so a dead row can still name a
-- running VM. Its lease is claimed here and the boot-time reconcile then tears
-- the VM down before releasing the slot — the fail-open hole in the old seed.
--
-- The addresses are derived with the same 10.201 arithmetic the code uses, and
-- vm_uid with the default APP_FC_VM_UID_BASE (100000). A host running a
-- non-default base would produce a lease whose uid disagrees with its live VMs —
-- which the boot-time reconcile detects and fails CLOSED on, rather than
-- silently fencing the wrong uid.
--
-- ON CONFLICT DO NOTHING with the oldest row first is deliberate: if two rows
-- somehow claim one index, the OLDER keeps it and the loser stays LEASELESS. A
-- leaseless occupying row is the collision SIGNAL the reconcile refuses to boot
-- on — it is not repaired here, because there is no safe way to guess which of
-- two rows describes the VM that is actually running.
INSERT INTO fleet_net_leases
    (host_id, host_ordinal, local_slot, net_index, host_ip, guest_ip, tap_name, vm_uid, owner_kind, owner_id, created_at, updated_at)
SELECT h.id,
       h.net_ordinal,
       r.net_index - (h.net_ordinal * 1024),
       r.net_index,
       '10.201.' || ((r.net_index * 4) / 256) || '.' || (((r.net_index * 4) % 256) + 1),
       '10.201.' || ((r.net_index * 4) / 256) || '.' || (((r.net_index * 4) % 256) + 2),
       'img' || (r.net_index - (h.net_ordinal * 1024)),
       100000 + (r.net_index - (h.net_ordinal * 1024)),
       'replica',
       r.id,
       r.created_at,
       now()
FROM fleet_replicas r
JOIN fleet_hosts h ON h.id = r.host_id
WHERE r.net_index > 0
  AND r.state IN ('booting', 'resident', 'paused', 'draining', 'dead')
  AND h.net_ordinal IS NOT NULL
  AND (r.net_index - (h.net_ordinal * 1024)) BETWEEN 1 AND 1023
ORDER BY r.created_at, r.id
ON CONFLICT DO NOTHING;

-- ── Backfill: leases for live image workloads ────────────────────────────────
-- image_workloads carry no host_id (the CP3 path is single-host by construction),
-- so they are attributed to the ordinal-0 host — the one they were booted on.
-- Same conflict rule: a workload that loses to a replica stays leaseless and the
-- reconcile refuses to boot on this host until an operator resolves it.
INSERT INTO fleet_net_leases
    (host_id, host_ordinal, local_slot, net_index, host_ip, guest_ip, tap_name, vm_uid, owner_kind, owner_id, created_at, updated_at)
SELECT h.id,
       h.net_ordinal,
       w.net_index,
       w.net_index,
       '10.201.' || ((w.net_index * 4) / 256) || '.' || (((w.net_index * 4) % 256) + 1),
       '10.201.' || ((w.net_index * 4) / 256) || '.' || (((w.net_index * 4) % 256) + 2),
       'img' || w.net_index,
       100000 + w.net_index,
       'workload',
       w.id,
       w.created_at,
       now()
FROM image_workloads w
JOIN fleet_hosts h ON h.net_ordinal = 0
WHERE w.net_index > 0
  AND w.state IN ('booting', 'running')
  AND w.net_index BETWEEN 1 AND 1023
ORDER BY w.created_at, w.id
ON CONFLICT DO NOTHING;
