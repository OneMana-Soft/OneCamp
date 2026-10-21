-- no-transaction
-- Foreign key cycle_unfinished_tasks.task_uuid -> tasks (ON DELETE CASCADE)
-- had no index, so deleting a tasks row scanned all of cycle_unfinished_tasks.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_cycle_unfinished_tasks_task_uuid ON cycle_unfinished_tasks (task_uuid);
