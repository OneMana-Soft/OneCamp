DROP INDEX IF EXISTS idx_code_pr_runs_agent_task;
ALTER TABLE code_pr_runs DROP COLUMN IF EXISTS agent_task_id;
