-- no-transaction
-- Foreign key oauth_requests.client_id -> oauth_clients (ON DELETE CASCADE)
-- had no index, so deleting a oauth_clients row scanned all of oauth_requests.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_oauth_requests_client_id ON oauth_requests (client_id);
