-- Enhance webhooks for Slack-quality bot/AI integration
-- Add response_url support for interactive messages, thread_ts, ephemeral, blocks

ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS response_url text;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS supports_blocks boolean DEFAULT false;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS supports_ephemeral boolean DEFAULT false;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS timeout_ms int DEFAULT 10000;

-- Add webhook_log indexes for faster admin queries
CREATE INDEX IF NOT EXISTS idx_webhook_logs_webhook_id_created ON webhook_logs(webhook_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_logs_success ON webhook_logs(success) WHERE success = false;
