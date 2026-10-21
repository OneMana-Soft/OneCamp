-- no-transaction
-- Foreign key ai_pending_actions.agent_id -> ai_agents (ON DELETE SET NULL)
-- had no index, so deleting a ai_agents row scanned all of ai_pending_actions.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_pending_actions_agent_id ON ai_pending_actions (agent_id);
