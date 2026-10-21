-- no-transaction
-- Foreign key event_reminders.user_id -> users (ON DELETE CASCADE); its primary key leads with event_id
-- had no index leading with it, so deleting a row of users scanned all of event_reminders.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_event_reminders_user_id ON event_reminders (user_id);
