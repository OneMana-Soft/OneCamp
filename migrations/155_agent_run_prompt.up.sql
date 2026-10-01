-- The prompt a run was given.
--
-- A run recorded what it DID (steps, tools, status, tokens, the composed prompt's
-- fingerprint) and never what it was ASKED. That is enough to review a run and
-- not enough to reuse one: an evaluation scenario is a prompt plus expectations,
-- so a failed run could not become the test that stops it happening again. The
-- experience was captured and could not be learned from.
--
-- Nullable, no backfill. Runs that predate this genuinely have no prompt on
-- record and inventing one would be worse than admitting it.
ALTER TABLE ai_agent_runs ADD COLUMN IF NOT EXISTS trigger_prompt TEXT;

-- The query the review surface makes: this agent's recent runs that ended badly
-- and still have the prompt needed to turn one into a scenario.
CREATE INDEX IF NOT EXISTS ai_agent_runs_agent_status_started_idx
    ON ai_agent_runs (agent_id, status, started_at DESC);
