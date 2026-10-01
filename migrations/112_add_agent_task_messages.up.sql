-- Migration 112: durable conversation state for agent tasks (step-replay
-- resume). The agent task queue (migration 111) already makes the JOB durable
-- (reclaim + retry across restarts). This adds the per-job CONVERSATION so a
-- run that paused (a needs_human blocker, a budget pause) or crashed mid-loop
-- resumes the SAME conversation with its full tool-result history, instead of
-- starting cold. The worker checkpoints the message list after each step; on
-- resume it feeds it back to the runner, which rebuilds its dedupe set from the
-- prior tool calls so an already-performed write is never repeated.
--
-- Additive + nullable-defaulted: an empty array means "no saved conversation"
-- (a fresh run from the prompt), so every existing job and code path is
-- unchanged.

ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS messages jsonb NOT NULL DEFAULT '[]'::jsonb;
