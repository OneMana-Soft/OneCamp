-- Rollback migration 135: drop the mid-run steering inbox. Undelivered messages
-- are lost with it (they only ever live here until a worker drains them), which
-- is why the down migration is safe to run but not silent — the agent simply
-- stops receiving instructions given while it works, as before.
ALTER TABLE ai_agent_tasks
    DROP COLUMN IF EXISTS steering;
