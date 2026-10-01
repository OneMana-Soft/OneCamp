DROP INDEX IF EXISTS idx_webhooks_signature_required;
DROP INDEX IF EXISTS idx_webhook_commands_webhook_id;
DROP INDEX IF EXISTS idx_webhook_commands_command;
DROP TABLE IF EXISTS webhook_commands;
ALTER TABLE webhooks DROP COLUMN IF EXISTS signature_required;
ALTER TABLE webhooks DROP COLUMN IF EXISTS commands;
ALTER TABLE webhooks DROP COLUMN IF EXISTS supports_interactive;
