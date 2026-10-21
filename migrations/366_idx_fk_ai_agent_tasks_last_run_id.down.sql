-- A plain DROP, not CONCURRENTLY: sqlx-cli 0.8 runs a revert in a transaction
-- whatever its first line says, and dropping an index is quick.
DROP INDEX IF EXISTS idx_fk_ai_agent_tasks_last_run_id;
