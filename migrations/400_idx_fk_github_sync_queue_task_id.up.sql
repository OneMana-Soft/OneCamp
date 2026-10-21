-- no-transaction
-- Foreign key github_sync_queue.task_id -> tasks (ON DELETE NO ACTION) had no
-- index, so deleting a tasks row scanned all of github_sync_queue.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_github_sync_queue_task_id ON github_sync_queue (task_id);
