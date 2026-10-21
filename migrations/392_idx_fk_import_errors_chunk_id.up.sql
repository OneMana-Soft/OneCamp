-- no-transaction
-- Foreign key import_errors.chunk_id -> import_chunks (ON DELETE SET NULL) had
-- no index, so deleting a import_chunks row scanned all of import_errors.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_import_errors_chunk_id ON import_errors (chunk_id);
