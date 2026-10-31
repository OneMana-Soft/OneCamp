-- no-transaction
-- Foreign key cycle_feeds.project_uuid -> projects (ON DELETE CASCADE): its only index leading with it,
-- cycle_feeds_live_idx, is partial (WHERE revoked_at IS NULL), so deleting a project scanned every
-- stopped link of cycle_feeds.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_cycle_feeds_project_uuid ON cycle_feeds (project_uuid);
