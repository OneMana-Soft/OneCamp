-- Migration 151: retention for the agent run ledger.
--
-- The larger of the two records by far, and the one with the stronger
-- minimisation argument: a run row carries the full transcript of what the model
-- and its tools said to each other, which is workspace content by another name.
-- The audit log got a retention window in 150 and this did not.
--
-- REDACT, NOT DELETE, for a different reason than the audit log. This table is
-- not hash-chained, so deleting rows would break no integrity guarantee. It
-- would break the NUMBERS: the acceptance record, the activity feed and the
-- token accounting are all computed over these rows, and deleting them would
-- quietly rewrite an agent's history to look better or worse than it was.
--
-- So the heavy, sensitive columns are cleared and the row stays. What survives
-- is what the metrics need and what a person can act on: status, counts, tokens,
-- timings. What goes is the transcript and the free text.
ALTER TABLE ai_agent_runs
    ADD COLUMN IF NOT EXISTS "redacted_at" TIMESTAMP WITH TIME ZONE;

CREATE INDEX IF NOT EXISTS idx_ai_agent_runs_retention
    ON ai_agent_runs (started_at)
    WHERE redacted_at IS NULL;
