ALTER TABLE webhooks DROP COLUMN IF EXISTS response_url;
ALTER TABLE webhooks DROP COLUMN IF EXISTS supports_blocks;
ALTER TABLE webhooks DROP COLUMN IF EXISTS supports_ephemeral;
ALTER TABLE webhooks DROP COLUMN IF EXISTS timeout_ms;

DROP INDEX IF EXISTS idx_webhook_logs_webhook_id_created;
DROP INDEX IF EXISTS idx_webhook_logs_success;
