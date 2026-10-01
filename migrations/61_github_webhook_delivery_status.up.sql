-- 61_github_webhook_delivery_status
--
-- Adds processing-state tracking to github_webhook_deliveries so a
-- failed first attempt doesn't permanently drop a webhook event.
--
-- Before this migration the dedup row was inserted on receipt, so any
-- panic / network error inside the async goroutine made GitHub's
-- automatic retry a no-op (same delivery_id, conflict, skip). The new
-- columns let the handler:
--   1. INSERT with status='processing' on receipt (idempotent).
--   2. Mark 'completed' only after successful processing.
--   3. Mark 'failed' with an error message on terminal failure.
-- Dedup now skips ONLY rows that are already 'completed'; rows in
-- 'processing'/'failed' allow GitHub's retry to re-run the work.

ALTER TABLE github_webhook_deliveries
    ADD COLUMN IF NOT EXISTS status varchar(20) NOT NULL DEFAULT 'completed',
    ADD COLUMN IF NOT EXISTS attempts int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS error_message text,
    ADD COLUMN IF NOT EXISTS processed_at timestamptz,
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT NOW();

-- Existing rows represent successfully processed deliveries (the only
-- path before this migration). Backfill processed_at so the reaper
-- doesn't pick them up.
UPDATE github_webhook_deliveries
   SET status = 'completed',
       processed_at = COALESCE(processed_at, received_at)
 WHERE status IS NULL OR status = 'completed';

-- A partial index on non-completed rows is what the retry reaper scans,
-- and it stays tiny because the steady-state population is "almost
-- everything is completed".
CREATE INDEX IF NOT EXISTS idx_github_webhook_deliveries_status_pending
    ON github_webhook_deliveries(received_at)
    WHERE status <> 'completed';
