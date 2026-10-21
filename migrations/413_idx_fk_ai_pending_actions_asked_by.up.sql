-- no-transaction
-- Foreign key ai_pending_actions.asked_by -> users (ON DELETE CASCADE) had no
-- index, so deleting a users row scanned all of ai_pending_actions.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_pending_actions_asked_by ON ai_pending_actions (asked_by);
