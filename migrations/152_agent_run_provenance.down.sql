ALTER TABLE ai_agent_runs
    DROP COLUMN IF EXISTS "skills_used",
    DROP COLUMN IF EXISTS "prompt_sha256",
    DROP COLUMN IF EXISTS "model";
