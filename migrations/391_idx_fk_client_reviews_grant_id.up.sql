-- no-transaction
-- Foreign key client_reviews.grant_id -> guest_grants (ON DELETE CASCADE) had
-- no index, so deleting a guest_grants row scanned all of client_reviews
-- (task_client_reviews until 352_docs_client_loop renamed it).
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_client_reviews_grant_id ON client_reviews (grant_id);
