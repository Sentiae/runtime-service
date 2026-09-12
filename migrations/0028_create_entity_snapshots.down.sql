-- Reverses 0028: drops entity_snapshots and, with it, its five indexes and the
-- primary key. On a database where the table predates 0028 (built by the retired
-- boot-time timetravel.AutoMigrate) this also drops the rows written before
-- 0028 — the same table, whoever created it.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

DROP TABLE IF EXISTS entity_snapshots;

COMMIT;
