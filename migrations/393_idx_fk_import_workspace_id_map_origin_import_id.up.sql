-- no-transaction
-- Foreign key import_workspace_id_map.origin_import_id -> import_jobs (ON
-- DELETE SET NULL) had no index, so deleting a import_jobs row scanned all of
-- import_workspace_id_map.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_import_workspace_id_map_origin_import_id ON import_workspace_id_map (origin_import_id);
