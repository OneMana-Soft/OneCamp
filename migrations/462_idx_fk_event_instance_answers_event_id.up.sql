-- no-transaction
-- Foreign key event_instance_answers.event_id -> events (ON DELETE CASCADE); its two unique indexes are partial on user_id, so neither serves event_id = $1
-- had no index leading with it, so deleting a row of events scanned all of event_instance_answers.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_event_instance_answers_event_id ON event_instance_answers (event_id);
