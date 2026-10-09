-- Migration 168: link each code_pr_runs row to the agent job that made it.
--
-- A follow-up on work the agent already turned into a pull request ("also
-- handle the empty list") ran as a fresh coding job, which opened a SECOND pull
-- request beside the first. To push the follow-up onto the pull request it
-- belongs to, the job has to find the pull request its own thread already has:
-- that is a lookup from the job (ai_agent_tasks: agent, source) to the runs it
-- produced. run_id cannot carry it (it references ai_agent_runs, and coding
-- jobs have none).
--
-- Additive and nullable, with no foreign key: finished jobs are pruned, and the
-- audit row must outlive them. Older rows stay NULL and simply are not
-- continued.
ALTER TABLE code_pr_runs
    ADD COLUMN IF NOT EXISTS agent_task_id uuid;

CREATE INDEX IF NOT EXISTS idx_code_pr_runs_agent_task
    ON code_pr_runs (agent_task_id)
    WHERE agent_task_id IS NOT NULL;
