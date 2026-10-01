-- Migration 117: per-agent "run tasks in the background" opt-in.
--
-- When true, a channel/DM @mention of this agent runs as a DURABLE job
-- (ai_agent_tasks) that shows evolving in-thread progress and survives
-- restarts, instead of a single synchronous pass. Default false, so every
-- existing agent keeps today's instant-reply behaviour until an owner opts in.
-- Agent CONFIG lives in Postgres (like dm_able, autonomy, enabled_tools); the
-- Dgraph graph is for the social layer (users/channels/posts), not agent config.
ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS run_in_background boolean NOT NULL DEFAULT false;
