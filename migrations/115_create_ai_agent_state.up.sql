-- Migration 115: per-agent durable "project state" blob — continue-the-work
-- across runs.
--
-- A run (ai_agent_runs) is one bounded execution; a scheduled agent re-runs from
-- scratch each tick with no memory of where it left off. This one-row-per-agent
-- scratchpad lets an agent advance long-running work slice-by-slice: the runner
-- injects the saved notes into the system prompt at the start of every run, and
-- the agent updates them via a generic save_progress tool. Generic across all
-- trigger kinds (scheduled, mention, task) — the same engine, one more durable
-- field.
--
-- Distinct from workspace memory (shared facts) and ai_agent_tasks.messages
-- (one job's conversation): this is the agent's own carry-forward state, not
-- tied to a single job or conversation.

CREATE TABLE IF NOT EXISTS ai_agent_state (
    "agent_id"   uuid PRIMARY KEY REFERENCES ai_agents(id) ON DELETE CASCADE,
    "state"      text NOT NULL DEFAULT '',
    "updated_at" timestamptz NOT NULL DEFAULT now()
);
