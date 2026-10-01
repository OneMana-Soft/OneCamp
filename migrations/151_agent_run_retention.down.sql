DROP INDEX IF EXISTS idx_ai_agent_runs_retention;
ALTER TABLE ai_agent_runs DROP COLUMN IF EXISTS "redacted_at";
