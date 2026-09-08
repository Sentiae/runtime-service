-- Reverses 0027. It REFUSES while any audit row exists: an egress audit row is
-- the only record of what a tenant's code reached — the sidecar that wrote it is
-- gone — so it has no other copy and cannot be re-derived, the rule 0024 and
-- 0025 already apply to their audit records. Recovery is forward.
SET statement_timeout = '60s';
SET lock_timeout = '5s';

BEGIN;

DO $$
DECLARE kept bigint;
BEGIN
    SELECT count(*) INTO kept FROM node_egress_decisions;
    IF kept > 0 THEN
        RAISE EXCEPTION 'refusing rollback: % node_egress_decisions row(s) are the only record of what a tenant''s node reached — recover forward or restore from a verified backup', kept;
    END IF;
END $$;

DROP TABLE IF EXISTS node_egress_decisions;

COMMIT;
