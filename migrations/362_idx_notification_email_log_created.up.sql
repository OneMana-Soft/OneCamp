-- no-transaction
-- The email log's retention delete runs by created_at alone; the existing
-- index leads with user_id.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_notification_email_log_created ON notification_email_log (created_at);
