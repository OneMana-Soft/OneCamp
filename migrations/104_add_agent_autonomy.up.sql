-- Migration 104: per-agent autonomy mode (governed autonomy / the "90/10 rule").
--
-- Lets an admin decide how much an agent may do on its own:
--   'auto'     -> the agent performs its write actions itself during a run
--                 (today's behavior; bounded by scope + permissions + budget).
--   'approval' -> the agent still does all the read/think work autonomously,
--                 but every WRITE is proposed as a durable pending action that a
--                 human must approve; the action then executes AS the agent's
--                 owner with permissions re-checked (reuses the in-thread
--                 approval mechanism). Read-only tools always run.
--
-- This is the governance control the 2026 agentic-AI consensus asks for:
-- automate the 90%, keep a human on the critical 10%. Additive + non-null with
-- a safe default, so every existing agent keeps its current behavior.
ALTER TABLE ai_agents ADD COLUMN IF NOT EXISTS "autonomy" text NOT NULL DEFAULT 'auto'
	CHECK ("autonomy" IN ('auto', 'approval'));
