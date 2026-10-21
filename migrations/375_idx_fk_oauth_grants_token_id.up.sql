-- no-transaction
-- Foreign key oauth_grants.token_id -> api_tokens (ON DELETE CASCADE) had no
-- index, so deleting a api_tokens row scanned all of oauth_grants.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_oauth_grants_token_id ON oauth_grants (token_id);
