-- no-transaction
-- V-184 head 4b: a custom AI provider's label is unique among live providers
-- only, so one added again beside a provider taken away (kept, marked) is a
-- new row. It replaces uq_ai_providers_label (lower(label) over every row),
-- which 560 drops once this one is built.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_ai_providers_label_live ON ai_providers (lower(label)) WHERE deleted_at IS NULL;
