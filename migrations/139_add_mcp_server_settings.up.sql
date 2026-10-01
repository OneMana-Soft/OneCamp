-- The governed MCP surface, admin-configurable.
--
-- /v1/mcp has been reachable by any api_token holding the right scope since it
-- shipped. That is a defensible default for a read-only integration API, and it is the
-- wrong default for a surface that external agents connect to: the workspace admin —
-- the person accountable for what agents do here — had no way to say whether MCP is
-- exposed at all, or which parts of the workspace it reaches.
--
-- These two columns give them both, in the same place every other AI governance control
-- already lives (ai_settings, hot-reloaded, editable without a redeploy), because a
-- second settings home is a second place to look when something is unexpectedly on.
--
-- WHAT THIS DOES NOT REPLACE. Enabling MCP does not widen anyone's permissions. A call
-- is still bounded by the token's scopes intersected with its owner's live permission on
-- the specific object. This is an admission control layer ON TOP of that: an admin can
-- narrow what is reachable, never broaden it. Turning everything on returns the surface
-- to exactly the authority it already had.

ALTER TABLE ai_settings
    -- Off by default, including for existing workspaces on upgrade.
    --
    -- Deliberately a behaviour CHANGE for anyone already using /v1/mcp, and the right
    -- one: the alternative is defaulting to true so nothing breaks, which would mean
    -- the control exists but was never actually decided by anybody. An admin turning it
    -- on is the point of having it. Release notes carry this; a silent default cannot.
    ADD COLUMN IF NOT EXISTS mcp_enabled boolean NOT NULL DEFAULT false,

    -- Which GROUPS of tools the surface exposes, as a comma-separated list of scope
    -- prefixes ('tasks', 'docs', 'messages', 'tables', 'search', ...), or '*' for all.
    --
    -- Empty means NOTHING, so enabling MCP alone exposes no tools until an admin names a
    -- group. Same shape as agent_delegation_surfaces, and for the same reason: an
    -- operator opts in to one area, watches the call log, then widens.
    --
    -- GROUPS RATHER THAN INDIVIDUAL TOOLS because the list has to stay comprehensible to
    -- the person deciding. "Can agents read our documents" is a question an admin can
    -- answer; "should read_doc be enabled but not summarize_channel" is one they will
    -- answer by enabling everything. The prefixes are derived from the existing scope
    -- names rather than declared separately, so a new tool joins an existing group
    -- automatically and cannot invent a group nobody has approved.
    --
    -- Text rather than a join table, matching agent_delegation_surfaces: a small
    -- operator allowlist read on the request hot path, not a relationship anything else
    -- needs to query.
    ADD COLUMN IF NOT EXISTS mcp_tool_groups text NOT NULL DEFAULT '';
