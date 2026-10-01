-- Reverting leaves delegation governed by the env vars alone, which is exactly
-- the state before this migration: the code treats a missing/false DB setting and
-- an absent env var identically (off), so a rollback disables delegation rather
-- than stranding it in an unknown state.
ALTER TABLE ai_settings
    DROP CONSTRAINT IF EXISTS ai_settings_agent_delegation_max_hops_range;

ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS agent_delegation_enabled,
    DROP COLUMN IF EXISTS agent_delegation_max_hops,
    DROP COLUMN IF EXISTS agent_delegation_surfaces;
