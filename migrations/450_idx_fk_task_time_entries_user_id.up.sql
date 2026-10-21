-- no-transaction
-- Foreign key task_time_entries.user_id -> users (ON DELETE CASCADE) had no
-- index, so deleting a users row scanned all of task_time_entries.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_task_time_entries_user_id ON task_time_entries (user_id);
