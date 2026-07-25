-- SentiaeDB — fix `#two-orgs-same-claim-key-share-one-database` (cross-tenant).
-- A dedicated resource's app was keyed 'resource/<claim_key>' with no org, so the
-- DERIVED ingress host (sanitizeSlug(component_id)-sanitizeSlug(env), unique-indexed
-- by 0006) collided across organisations too: with 0014 alone, org B's provision of
-- an already-claimed key would fail on a duplicate host instead of working. Putting
-- the owning org inside the component id makes both keys org-local.

UPDATE fleet_apps a
   SET component_id = 'resource/' || res.owner_org::text || '/' || res.claim_key
  FROM fleet_resources res
 WHERE res.app_id = a.id
   AND a.component_id = 'resource/' || res.claim_key;

-- Delete rather than rewrite host_pattern: the slug rule lives in Go
-- (sanitizeSlug, internal/usecase/fleet_orchestrator.go) and re-implementing it in
-- SQL would fork the single source of truth. ensureRoute is idempotent — it
-- recreates the route from the CURRENT component id when an app has none — so
-- deleting is the correct, drift-free move.
DELETE FROM fleet_routes r
 USING fleet_apps a, fleet_resources res
 WHERE r.app_id = a.id
   AND res.app_id = a.id
   AND a.component_id = 'resource/' || res.owner_org::text || '/' || res.claim_key;
