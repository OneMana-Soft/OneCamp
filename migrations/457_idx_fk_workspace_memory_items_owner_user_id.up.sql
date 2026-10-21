-- no-transaction
-- Foreign key workspace_memory_items.owner_user_id -> users (ON DELETE SET
-- NULL) had no index, so deleting a users row scanned all of
-- workspace_memory_items.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_workspace_memory_items_owner_user_id ON workspace_memory_items (owner_user_id);
