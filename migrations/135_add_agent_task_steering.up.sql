-- Migration 135: deliver a human's mid-run instruction to a working agent.
--
-- When someone replied while an agent was already working, the reply went
-- nowhere. ContinueAgentWork treated a 'running' job as "already in flight",
-- suppressed a duplicate launch, and dropped the message — so "actually, use the
-- other repo" or "skip the migration" arrived only after the agent had finished
-- doing it the wrong way. The workaround was to stop the run and re-ask, losing
-- everything it had done.
--
-- This column is the inbox for those messages. It is deliberately NOT the
-- existing `messages` conversation: a live run holds the conversation in memory
-- and checkpoints it after every step, so appending a turn there would be
-- overwritten by the next checkpoint (or worse, interleave with it). Steering is
-- append-only for humans and drained by the ONE worker that holds the lease,
-- which then folds the messages into the run's conversation between steps —
-- never mid-turn, so a tool result is never separated from the call it answers.
--
-- Shape: a JSON array of {at, by, text}. Bounded server-side (a handful of
-- entries, each length-capped) so a comment burst can't inflate the row or the
-- next prompt. A queued job can be steered too: the worker drains before its
-- first model call, so a reply that lands in the gap between enqueue and claim
-- is delivered rather than lost.
-- Idempotent.

ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS steering jsonb NOT NULL DEFAULT '[]'::jsonb;
