-- no-transaction
-- Foreign key sandbox_runs.run_id -> ai_agent_runs (ON DELETE SET NULL) had no
-- index, so deleting a ai_agent_runs row scanned all of sandbox_runs.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_sandbox_runs_run_id ON sandbox_runs (run_id);
