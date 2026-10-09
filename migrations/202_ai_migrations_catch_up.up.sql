-- Migration 202: what migrations 168, 169, 170 and 172 add, where it is missing.
--
-- Both edition lines carry the same migration set (docs/Releasing.md), but the
-- without-ai line went out without those four. A database migrates only
-- forward past its version, so one that ran without-ai beyond 172 never got
-- them, and switching it to the AI edition would leave agent jobs, the action
-- log and model routing without their columns. This adds each one only where
-- it is absent: on a database that ran them it changes nothing.

-- 168: each code_pr_runs row names the agent job that made it.
ALTER TABLE code_pr_runs
    ADD COLUMN IF NOT EXISTS agent_task_id uuid;

CREATE INDEX IF NOT EXISTS idx_code_pr_runs_agent_task
    ON code_pr_runs (agent_task_id)
    WHERE agent_task_id IS NOT NULL;

-- 169: the work sessions a durable agent job has used.
ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS sessions int NOT NULL DEFAULT 1;

-- 170: each agent action carries its agent's signature.
ALTER TABLE ai_agent_action_log
    ADD COLUMN IF NOT EXISTS signature bytea;

-- 172: which model does which kind of background work.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS model_routing jsonb NOT NULL DEFAULT '{}'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_settings_model_routing_is_object') THEN
        ALTER TABLE ai_settings
            ADD CONSTRAINT ai_settings_model_routing_is_object
                CHECK (jsonb_typeof(model_routing) = 'object');
    END IF;
END $$;
