-- Reverting removes the admission control, returning /v1/mcp to "reachable by any token
-- holding the scope" — which is the state before this migration.
--
-- Worth being explicit that a rollback WIDENS the surface rather than narrowing it, the
-- opposite direction from most reverts. The code reads a missing column as an error
-- rather than as false, so an old binary against a new schema is the safe mismatch and a
-- new binary against an old schema fails its startup schema check instead of silently
-- treating the gate as absent.
ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS mcp_enabled,
    DROP COLUMN IF EXISTS mcp_tool_groups;
