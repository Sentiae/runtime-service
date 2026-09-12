-- D-490 tier 0A — entity_snapshots becomes a versioned migration.
--
-- Until now this table was created at every boot by timetravel.AutoMigrate
-- (platform-kit/timetravel/recorder.go), called from the DI container through
-- the SERVING pool, with its error logged and ignored. That was real DDL issued
-- by the serving role behind the migration authority's back — the one thing
-- the owner/app split forbids. The call is deleted; this file is the table's
-- only author now.
--
-- The shape is AutoMigrate's, verbatim: it was captured by running
-- timetravel.AutoMigrate (platform-kit v0.3.36) against a scratch Postgres 16
-- and reading pg_dump --schema-only, and it matches the entity_snapshots
-- sections of the foundry/git/work/canvas/ops baselines, which were dumped the
-- same way. Same column types, same defaults, same primary-key constraint name,
-- same five index names — so on a database where AutoMigrate already built the
-- table (runtime_service and runtime_service_fc both ran the same boot path)
-- every statement below is IF NOT EXISTS and changes nothing.
--
-- ⚠ IF NOT EXISTS still checks privilege first: CREATE INDEX resolves the table
-- with an ownership check BEFORE it looks for the index name. On a database
-- where AutoMigrate built this table as another role, the table must be owned
-- by the role running this migration before it runs (tier 0B re-owns every
-- existing table to the service's _owner role).
--
-- squawk: varchar(n) is kept because the columns must equal the existing ones —
-- a text column here would give fresh databases a different shape from every
-- database that already has the table. That is what the four line-scoped
-- prefer-text-field ignores buy, and nothing else: IF NOT EXISTS never alters
-- an existing column, so text here could only ever create a second shape. It is spelled varchar/timestamptz, the
-- same types pg_dump prints as character varying/timestamp with time zone,
-- because squawk's ban-char-field reads the word "character" as char(n). The
-- indexes are on a table created in this same transaction, so CONCURRENTLY is
-- neither needed nor allowed.
--
-- Numbering: 0028 is next-free (0026's note on m.Up() and skipped versions).
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

CREATE TABLE IF NOT EXISTS entity_snapshots (
    id             uuid         NOT NULL,
    -- squawk-ignore prefer-text-field
    kind           varchar(64)  NOT NULL,
    -- squawk-ignore prefer-text-field
    entity_id      varchar(128) NOT NULL,
    payload        jsonb        NOT NULL,
    valid_from     timestamptz  NOT NULL,
    valid_to       timestamptz,
    -- squawk-ignore prefer-text-field
    writer_service varchar(64)  NOT NULL,
    changed_by     uuid         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'::uuid,
    -- squawk-ignore prefer-text-field
    change_reason  varchar(255) NOT NULL DEFAULT 'system'::varchar,
    created_at     timestamptz  NOT NULL,
    CONSTRAINT entity_snapshots_pkey PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_entity_snapshots_changed_by     ON entity_snapshots USING btree (changed_by);
CREATE INDEX IF NOT EXISTS idx_entity_snapshots_kind_id        ON entity_snapshots USING btree (kind, entity_id);
CREATE INDEX IF NOT EXISTS idx_entity_snapshots_valid_from     ON entity_snapshots USING btree (valid_from);
CREATE INDEX IF NOT EXISTS idx_entity_snapshots_valid_to       ON entity_snapshots USING btree (valid_to);
CREATE INDEX IF NOT EXISTS idx_entity_snapshots_writer_service ON entity_snapshots USING btree (writer_service);

COMMIT;
