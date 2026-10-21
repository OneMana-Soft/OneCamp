-- no-transaction
-- Foreign key webhooks.channel_id -> channels (ON DELETE NO ACTION) had no
-- index, so deleting a channels row scanned all of webhooks.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_webhooks_channel_id ON webhooks (channel_id);
