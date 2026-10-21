-- no-transaction
-- Foreign key archive_policies.created_by -> users (ON DELETE NO ACTION) had
-- no index, so deleting a users row scanned all of archive_policies.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_archive_policies_created_by ON archive_policies (created_by);
