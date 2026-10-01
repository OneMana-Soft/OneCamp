DROP INDEX IF EXISTS idx_ai_pending_actions_run;
DROP INDEX IF EXISTS idx_ai_pending_actions_agent_outcome;
ALTER TABLE ai_pending_actions
    DROP COLUMN IF EXISTS "run_id",
    DROP COLUMN IF EXISTS "agent_id";
