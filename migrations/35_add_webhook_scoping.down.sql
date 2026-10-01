ALTER TABLE webhooks DROP COLUMN IF EXISTS trigger_words;
ALTER TABLE webhooks DROP COLUMN IF EXISTS target_type;
ALTER TABLE webhooks DROP COLUMN IF EXISTS scope_entity_id;
ALTER TABLE webhooks DROP COLUMN IF EXISTS scope_type;
