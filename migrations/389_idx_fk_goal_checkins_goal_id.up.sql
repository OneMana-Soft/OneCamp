-- no-transaction
-- Foreign key goal_checkins.goal_id -> goals (ON DELETE CASCADE) had no index,
-- so deleting a goals row scanned all of goal_checkins.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_goal_checkins_goal_id ON goal_checkins (goal_id);
