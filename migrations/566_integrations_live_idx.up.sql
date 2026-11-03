-- no-transaction
-- V-184 head 5b: an integration is unique per entity and provider among
-- connected rows only, so connecting again after a disconnect makes a new row
-- beside the one kept, marked. It replaces
-- integrations_entity_type_entity_id_provider_key (every row), which 567 drops
-- once this one is built.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_integrations_live ON integrations (entity_type, entity_id, provider) WHERE deleted_at IS NULL;
