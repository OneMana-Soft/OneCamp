-- no-transaction
-- Dropping push tokens the provider rejected finds them by fcm_token = ANY;
-- the unique index leads with user_id.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_users_fcm_token_token ON users_fcm_token (fcm_token);
