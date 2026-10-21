-- no-transaction
-- Foreign key users_channel_notification.channel_id -> channels (ON DELETE NO
-- ACTION) had no index, so deleting a channels row scanned all of
-- users_channel_notification.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_users_channel_notification_channel_id ON users_channel_notification (channel_id);
