-- The full unique counts every row, so the integrations disconnected go first:
-- one connected again would collide.
DELETE FROM integrations WHERE deleted_at IS NOT NULL;
ALTER TABLE integrations ADD CONSTRAINT integrations_entity_type_entity_id_provider_key UNIQUE (entity_type, entity_id, provider);
