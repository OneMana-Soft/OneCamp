-- no-transaction
-- Grants of a resource read by resource_id with revoked grants included
-- (models/postgres/Guest: any grant ever, the first grant); the active index
-- is partial on revoked_at IS NULL.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_guest_grants_resource ON guest_grants (resource_id);
