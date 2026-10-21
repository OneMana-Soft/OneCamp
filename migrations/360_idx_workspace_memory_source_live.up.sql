-- no-transaction
-- Editing or deleting a message finds its memory items by source; the only
-- source index was partial on deleted rows.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_workspace_memory_source_live ON workspace_memory_items (source_type, source_uuid) WHERE deleted_at IS NULL;
