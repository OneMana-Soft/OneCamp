-- no-transaction
-- Foreign key goals.parent_id -> goals (ON DELETE NO ACTION) had no index, so
-- deleting a goals row scanned all of goals.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_goals_parent_id ON goals (parent_id);
