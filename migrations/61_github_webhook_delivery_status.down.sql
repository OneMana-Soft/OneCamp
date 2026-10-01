DROP INDEX IF EXISTS idx_github_webhook_deliveries_status_pending;

ALTER TABLE github_webhook_deliveries
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS attempts,
    DROP COLUMN IF EXISTS error_message,
    DROP COLUMN IF EXISTS processed_at,
    DROP COLUMN IF EXISTS updated_at;
