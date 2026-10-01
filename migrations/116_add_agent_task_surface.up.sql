-- Migration 116: reply-surface descriptor for durable agent tasks.
--
-- The durable agent-task queue (migration 111) today drives one kind of work: a
-- project task assigned to an agent, whose status is posted as a comment on
-- that task. To let the SAME durable engine also drive a channel post thread, a
-- group chat, or a 1:1 DM (async long-running @mentions), a job carries a small
-- JSON descriptor of WHERE it should post/edit its evolving status comment:
--   {"kind":"channel_post","channel_id":"…","post_id":"…"}
--   {"kind":"group_chat","group_id":"…","message_id":"…"}
--   {"kind":"dm","group_id":"…","message_id":"…"}
--
-- Additive + nullable: NULL/absent means the legacy task surface (kind "task"),
-- so every existing job row and code path is unchanged. The worker decodes this
-- column and selects a per-surface status poster; a blank/malformed value
-- safely decodes to the task surface.
ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS surface jsonb;
