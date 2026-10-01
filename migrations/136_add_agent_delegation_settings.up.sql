-- Agent-to-agent delegation, admin-configurable.
--
-- Delegation was shipped behind env vars (AI_AGENT_DELEGATION,
-- AI_AGENT_DELEGATION_CHANNELS, AI_AGENT_DELEGATION_MAX_HOPS) because the
-- behaviour had never been exercised and an env knob can be changed without a
-- schema migration. Now that the mechanism is complete it belongs where every
-- other AI governance control already lives: ai_settings, hot-reloaded, editable
-- by an admin without a redeploy.
--
-- Env is retained as a DEPLOYMENT-LEVEL KILL SWITCH rather than removed. A
-- self-hoster who has decided agents must never talk to each other should be able
-- to enforce that from infrastructure, without depending on nobody flipping a
-- toggle in the UI. Precedence is therefore: env OFF wins over any admin setting;
-- otherwise the admin setting governs.

ALTER TABLE ai_settings
    -- Off by default. Enabling this changes WHO can cause an agent to spend money
    -- (one agent can now cause another to run), so it is never on implicitly.
    ADD COLUMN IF NOT EXISTS agent_delegation_enabled  boolean NOT NULL DEFAULT false,

    -- How many agent turns deep a chain may go. 2 covers the motivating case,
    -- human -> triage -> coder, without allowing an open-ended relay. Bounded in
    -- the DB as well as in code so a bad write cannot uncap the budget: 0 would
    -- silently disable a feature the admin enabled, and a large value is the
    -- expensive failure mode.
    ADD COLUMN IF NOT EXISTS agent_delegation_max_hops integer NOT NULL DEFAULT 2
        CONSTRAINT ai_settings_agent_delegation_max_hops_range CHECK (agent_delegation_max_hops BETWEEN 1 AND 5),

    -- Where collaboration is permitted, as a comma-separated list of SURFACE keys
    -- ('<channel-uuid>' or 'task:<task-uuid>'), or '*' for everywhere. Empty means
    -- nowhere, so enabling the feature alone does nothing until a surface is
    -- named — an admin opts in one place, watches it, then widens.
    --
    -- Stored as text rather than a join table on purpose: it is a small operator
    -- allowlist read on the message hot path, not a relationship anything else
    -- needs to query, and the surface ids span two id spaces (channels and tasks)
    -- that no single foreign key could cover.
    ADD COLUMN IF NOT EXISTS agent_delegation_surfaces text NOT NULL DEFAULT '';
