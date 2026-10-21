-- no-transaction
-- Foreign key ai_agent_tasks.run_as_user_id -> users (ON DELETE SET NULL) had
-- no index, so deleting a users row scanned all of ai_agent_tasks.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_agent_tasks_run_as_user_id ON ai_agent_tasks (run_as_user_id);
