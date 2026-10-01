-- Migration 122: opt-in ambient mode for AI agents.
--
-- Ambient lets an agent reply in its scoped channels WITHOUT being @mentioned,
-- when it judges it can add clear value — Claude Tag's "may respond to a message
-- it judges warrants a reply". OFF by default and gated hard: it only fires for
-- agents an admin explicitly opts in AND that have a channel scope, is
-- rate-limited per (agent, channel), budget-metered, and the agent self-selects
-- (stays silent unless genuinely useful). ambient_keywords narrows candidacy to
-- messages about the agent's topic (comma/newline separated; empty = questions
-- only). Idempotent.

ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS ambient          boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS ambient_keywords text    NOT NULL DEFAULT '';
