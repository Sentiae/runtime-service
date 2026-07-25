-- SentiaeDB Phase 0 — a durable resource whose PROTECTION has stopped must not
-- look healthy.
--
-- A failed SnapshotAppVolumes on a live durable resource was, until now, only a
-- returned error: no error-level signal an operator watches, and nothing durable
-- on the resource itself. So a resource whose snapshots had been failing for a
-- week read exactly like one snapshotted an hour ago.
--
-- consecutive_snapshot_failures is a COUNT, not a flag: a transient blip and a
-- protection outage must be distinguishable, and only the count (paired with
-- last_snapshot_success_at) tells them apart. An alert keys on the AGE of
-- last_snapshot_success_at plus the count; the status path turns a non-zero count
-- into a stable condition token.
--
-- last_snapshot_error holds the underlying cause for an operator. It is NOT
-- surfaced on the tenant-visible status (that carries the condition token), the
-- same split last_error already follows.
--
-- All four are additive with constant defaults (INTEGER NOT NULL DEFAULT 0, TEXT
-- NOT NULL DEFAULT '', nullable TIMESTAMPTZ) → no table rewrite, no lock beyond
-- the catalog update (squawk-safe).

ALTER TABLE fleet_resources
    ADD COLUMN consecutive_snapshot_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_snapshot_failure_at TIMESTAMPTZ,
    ADD COLUMN last_snapshot_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_snapshot_success_at TIMESTAMPTZ;
