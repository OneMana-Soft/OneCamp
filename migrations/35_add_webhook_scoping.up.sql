ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS scope_type varchar DEFAULT 'org' CHECK (scope_type IN ('org', 'channel', 'dm', 'group_chat', 'project'));
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS scope_entity_id uuid;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS target_type varchar DEFAULT 'channel' CHECK (target_type IN ('channel', 'dm', 'group_chat'));
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS trigger_words text[];
UPDATE webhooks SET scope_type = 'org' WHERE scope_type IS NULL;
UPDATE webhooks SET target_type = 'channel' WHERE target_type IS NULL;
