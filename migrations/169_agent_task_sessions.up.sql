-- Migration 169: count the work sessions a durable agent job has used.
--
-- A job whose run reached its step limit used to end there with "Re-assign
-- this to me to continue", and the re-assigned job started from nothing,
-- repeating every read the first one did. The job keeps its conversation, so it
-- can simply carry on in a new session instead. sessions bounds how often it
-- may (together with the agent's daily budget), so a job that is getting
-- nowhere still stops.
ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS sessions int NOT NULL DEFAULT 1;
