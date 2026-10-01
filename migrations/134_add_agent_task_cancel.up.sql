-- Migration 134: let a human stop an AI teammate's in-flight work.
--
-- A durable agent job could be started but never stopped: once queued it ran to
-- a terminal state (or sat blocked) regardless of whether the person who asked
-- still wanted it. That is a gap in both product and governance terms — "stop"
-- is table stakes for an agent that acts on real data, and an operator needs a
-- way to halt a misdirected run without waiting for a wall-clock limit or
-- restarting the worker.
--
-- Cancellation is COOPERATIVE, which is why it is a request rather than a state
-- flip: the worker owns the lease and the in-flight run, so a stop is recorded
-- here, observed by the lease heartbeat, and the worker unwinds its own run and
-- writes the terminal row. That keeps exactly one writer per lease (no torn
-- state) and lets the agent leave an honest trail of what it had already done.
--
--   cancel_requested_at  when a stop was asked for (NULL = no request)
--   cancel_requested_by  who asked, for the audit trail and the status message
--
-- A job that is NOT actively leased (queued / awaiting_input) has nothing in
-- flight, so the request and the terminal transition happen together; only a
-- 'running' job waits for its worker.
--
-- The state CHECK gains 'cancelled': a stopped job is neither 'done' (it did not
-- finish the work) nor 'failed' (nothing went wrong), and conflating it with
-- either would corrupt every reliability rollup built on these states. The
-- constraint is replaced rather than extended because Postgres has no "add value
-- to CHECK"; the new one is a strict superset, so no existing row can violate it.
-- Idempotent.

ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS cancel_requested_at timestamptz,
    ADD COLUMN IF NOT EXISTS cancel_requested_by uuid;

ALTER TABLE ai_agent_tasks
    DROP CONSTRAINT IF EXISTS ai_agent_tasks_state_check;

ALTER TABLE ai_agent_tasks
    ADD CONSTRAINT ai_agent_tasks_state_check
    CHECK (state IN ('queued', 'running', 'awaiting_input', 'done', 'failed', 'cancelled'));

-- Reclaim path: a job whose worker died with a pending stop must be finalized as
-- cancelled instead of requeued, so the index the reclaimer scans includes the
-- flag it now has to read.
CREATE INDEX IF NOT EXISTS idx_ai_agent_tasks_cancel_pending
    ON ai_agent_tasks (cancel_requested_at)
    WHERE cancel_requested_at IS NOT NULL AND state IN ('queued', 'running', 'awaiting_input');
