-- no-transaction
-- Foreign key ai_agents.bot_user_id -> users (ON DELETE NO ACTION) had no
-- index, so deleting a users row scanned all of ai_agents.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_agents_bot_user_id ON ai_agents (bot_user_id);
