-- no-transaction
-- Foreign key doc_export_consents.owner_uuid -> users (ON DELETE CASCADE) had
-- no index, so deleting a users row scanned all of doc_export_consents.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_doc_export_consents_owner_uuid ON doc_export_consents (owner_uuid);
