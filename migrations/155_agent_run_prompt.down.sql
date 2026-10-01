DROP INDEX IF EXISTS ai_agent_runs_agent_status_started_idx;
ALTER TABLE ai_agent_runs DROP COLUMN IF EXISTS trigger_prompt;
