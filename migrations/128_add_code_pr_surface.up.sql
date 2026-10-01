-- Migration 128: persist the reply-surface descriptor on code_pr_runs so a
-- comment/review on an agent-opened PR can be mapped back to the OneCamp thread
-- the agent posted to, and its work continued there.
--
-- The code-PR job (ai_agent_tasks) already carries the surface, but it reaches a
-- terminal state after opening the PR, and a GitHub webhook only knows the PR
-- URL — not the OneCamp post/message/task it originated from. Recording the
-- surface alongside the PR URL closes that loop generically (the same jsonb
-- shape used on ai_agent_tasks.surface: {kind, channel_id, post_id, ...}).
--
-- Additive + nullable: existing rows and every non-PR-comment code path are
-- unchanged. A partial index on pr_url keeps the PR-URL lookup cheap (the
-- outcome + comment webhooks both probe by pr_url on every closed/commented PR).
ALTER TABLE code_pr_runs
    ADD COLUMN IF NOT EXISTS surface jsonb;

CREATE INDEX IF NOT EXISTS idx_code_pr_runs_pr_url
    ON code_pr_runs (pr_url)
    WHERE pr_url <> '';
