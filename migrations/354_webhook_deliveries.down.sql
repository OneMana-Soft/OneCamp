-- Migration 361, undone: webhooks go back to being sent from memory.
ALTER TABLE webhooks DROP COLUMN IF EXISTS "paused_reason";
ALTER TABLE webhooks DROP COLUMN IF EXISTS "paused_at";
DROP TABLE IF EXISTS webhook_deliveries;
