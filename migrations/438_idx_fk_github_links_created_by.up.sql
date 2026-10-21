-- no-transaction
-- Foreign key github_links.created_by -> users (ON DELETE NO ACTION) had no
-- index, so deleting a users row scanned all of github_links.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_github_links_created_by ON github_links (created_by);
