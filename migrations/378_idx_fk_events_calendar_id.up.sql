-- no-transaction
-- Foreign key events.calendar_id -> calendars (ON DELETE CASCADE) had no
-- index, so deleting a calendars row scanned all of events.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_events_calendar_id ON events (calendar_id);
