-- no-transaction
-- Foreign key code_pr_runs.run_id -> ai_agent_runs (ON DELETE SET NULL) had no
-- index, so deleting a ai_agent_runs row scanned all of code_pr_runs.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_code_pr_runs_run_id ON code_pr_runs (run_id);
