-- no-transaction
-- Foreign key capability_policies.updated_by -> users (ON DELETE SET NULL) had
-- no index, so deleting a users row scanned all of capability_policies.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_capability_policies_updated_by ON capability_policies (updated_by);
