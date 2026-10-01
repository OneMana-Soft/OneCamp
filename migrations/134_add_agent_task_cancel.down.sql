-- Rollback migration 134: drop cancellation support.
--
-- Any row already stopped by a human would violate the narrower CHECK, so those
-- rows are settled as 'failed' (the closest legacy terminal state) before the
-- constraint is restored — a rollback must not leave the table unconstrainable.
UPDATE ai_agent_tasks SET state = 'failed' WHERE state = 'cancelled';

ALTER TABLE ai_agent_tasks
    DROP CONSTRAINT IF EXISTS ai_agent_tasks_state_check;

ALTER TABLE ai_agent_tasks
    ADD CONSTRAINT ai_agent_tasks_state_check
    CHECK (state IN ('queued', 'running', 'awaiting_input', 'done', 'failed'));

DROP INDEX IF EXISTS idx_ai_agent_tasks_cancel_pending;

ALTER TABLE ai_agent_tasks
    DROP COLUMN IF EXISTS cancel_requested_at,
    DROP COLUMN IF EXISTS cancel_requested_by;
