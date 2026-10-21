-- no-transaction
-- Foreign key password_reset_tokens.user_id -> users (ON DELETE NO ACTION) had
-- no index, so deleting a users row scanned all of password_reset_tokens.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_password_reset_tokens_user_id ON password_reset_tokens (user_id);
