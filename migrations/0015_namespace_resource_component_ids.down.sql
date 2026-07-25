-- Reverse of 0015_namespace_resource_component_ids.up.sql — strip the org segment
-- back out of the resource component ids.

UPDATE fleet_apps a
   SET component_id = 'resource/' || res.claim_key
  FROM fleet_resources res
 WHERE res.app_id = a.id
   AND a.component_id = 'resource/' || res.owner_org::text || '/' || res.claim_key;

-- Same reasoning as the up migration: routes are deleted, not rewritten, so
-- ensureRoute rebuilds them from the (now un-namespaced) component id.
DELETE FROM fleet_routes r
 USING fleet_apps a, fleet_resources res
 WHERE r.app_id = a.id
   AND res.app_id = a.id
   AND a.component_id = 'resource/' || res.claim_key;
