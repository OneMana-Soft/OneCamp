-- Migration 107: add the 'plan' autonomy mode (plan-approve execution).
--
--   'plan' -> the agent does all read/think work autonomously, then proposes
--             its FULL ordered plan of writes as a SINGLE durable approval. On
--             approval the whole plan executes AS the owner, step by step, each
--             with permissions re-checked; on reject/expire nothing is written.
--             This is the "approve the whole thing once, never get surprised"
--             control (Miro-Sidekick-style), layered on the existing per-action
--             approval primitive.
--
-- Additive: widens the autonomy CHECK; existing 'auto'/'approval' agents are
-- unaffected. Idempotent.

ALTER TABLE ai_agents DROP CONSTRAINT IF EXISTS ai_agents_autonomy_check;
ALTER TABLE ai_agents ADD CONSTRAINT ai_agents_autonomy_check
	CHECK ("autonomy" IN ('auto', 'approval', 'plan'));
