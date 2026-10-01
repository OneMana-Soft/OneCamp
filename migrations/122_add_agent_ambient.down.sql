-- Rollback migration 122: drop ambient mode from agents.
ALTER TABLE ai_agents
    DROP COLUMN IF EXISTS ambient_keywords,
    DROP COLUMN IF EXISTS ambient;
