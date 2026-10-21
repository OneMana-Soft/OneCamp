-- no-transaction
-- Foreign key api_tokens.app_id -> apps (ON DELETE CASCADE); idx_api_tokens_app is partial on revoked_at IS NULL, so deleting an app scanned for its revoked tokens
-- had no index leading with it, so deleting a row of apps scanned all of api_tokens.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_api_tokens_app_id ON api_tokens (app_id);
