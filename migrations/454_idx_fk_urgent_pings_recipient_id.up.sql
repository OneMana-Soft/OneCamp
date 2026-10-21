-- no-transaction
-- Foreign key urgent_pings.recipient_id -> users (ON DELETE CASCADE) had no
-- index, so deleting a users row scanned all of urgent_pings.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_urgent_pings_recipient_id ON urgent_pings (recipient_id);
