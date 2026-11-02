-- no-transaction
-- A task.updated webhook says what each changed field was before (linear-39
-- round 2, Linear's updatedFrom), read from the task's own earlier changes,
-- newest first (business/WorkFeed Replaced). task_changes had only its
-- sequence and changed_at, so that read scanned the log.
-- CONCURRENTLY: task_changes is written on every task change, so the index is
-- built without blocking writes; one statement per file, outside a
-- transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_task_changes_task ON task_changes (resource_uuid, sequence);
