-- A remote brain can speak AG-UI (as before) or A2A. remote_card keeps what the
-- A2A agent card said (name, skills, provider, protocol version), so the agent
-- list can show it and a run knows which dialect to speak without refetching.
ALTER TABLE ai_agents ADD COLUMN IF NOT EXISTS remote_protocol varchar NOT NULL DEFAULT 'agui';
ALTER TABLE ai_agents DROP CONSTRAINT IF EXISTS ai_agents_remote_protocol_check;
ALTER TABLE ai_agents ADD CONSTRAINT ai_agents_remote_protocol_check CHECK (remote_protocol IN ('agui', 'a2a'));
ALTER TABLE ai_agents ADD COLUMN IF NOT EXISTS remote_card jsonb;
