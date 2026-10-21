-- no-transaction
-- Foreign key bookings.page_id -> booking_pages (ON DELETE RESTRICT) had no
-- index, so deleting a booking_pages row scanned all of bookings.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_bookings_page_id ON bookings (page_id);
