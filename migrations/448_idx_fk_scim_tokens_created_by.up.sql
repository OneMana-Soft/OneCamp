-- no-transaction
-- Foreign key scim_tokens.created_by -> users (ON DELETE SET NULL) had no
-- index, so deleting a users row scanned all of scim_tokens.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_scim_tokens_created_by ON scim_tokens (created_by);
