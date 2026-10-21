-- no-transaction
-- Foreign key task_recurrences.created_by -> users (ON DELETE SET NULL) had no
-- index, so deleting a users row scanned all of task_recurrences.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_task_recurrences_created_by ON task_recurrences (created_by);
