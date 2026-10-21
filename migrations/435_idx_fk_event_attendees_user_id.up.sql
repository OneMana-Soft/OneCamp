-- no-transaction
-- Foreign key event_attendees.user_id -> users (ON DELETE CASCADE) had no
-- index, so deleting a users row scanned all of event_attendees.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_event_attendees_user_id ON event_attendees (user_id);
