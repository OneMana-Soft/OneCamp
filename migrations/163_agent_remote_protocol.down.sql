ALTER TABLE ai_agents DROP CONSTRAINT IF EXISTS ai_agents_remote_protocol_check;
ALTER TABLE ai_agents DROP COLUMN IF EXISTS remote_card;
ALTER TABLE ai_agents DROP COLUMN IF EXISTS remote_protocol;
