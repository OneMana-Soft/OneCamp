-- no-transaction
-- Foreign key event_instance_answers.user_id -> users (ON DELETE CASCADE)
-- had no index leading with it, so deleting a row of users scanned all of event_instance_answers.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_event_instance_answers_user_id ON event_instance_answers (user_id);
