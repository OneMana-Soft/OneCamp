-- V-184 head 5b: the full unique gives way to 566's partial one, which holds
-- the same keys among connected rows only. A full unique counts a disconnected
-- integration (kept, marked), so connecting again would collide with it, and
-- the upserts' ON CONFLICT would write over the kept row instead of making a
-- new one. Dropping builds nothing, so it runs in this migration's transaction.
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_entity_type_entity_id_provider_key;
