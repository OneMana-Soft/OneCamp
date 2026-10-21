-- no-transaction
-- Foreign key ai_agent_eval_scenarios.created_by -> users (ON DELETE CASCADE)
-- had no index, so deleting a users row scanned all of
-- ai_agent_eval_scenarios.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_agent_eval_scenarios_created_by ON ai_agent_eval_scenarios (created_by);
