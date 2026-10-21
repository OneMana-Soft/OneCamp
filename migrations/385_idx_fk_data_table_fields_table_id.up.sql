-- no-transaction
-- Foreign key data_table_fields.table_id -> data_tables (ON DELETE CASCADE)
-- had no index, so deleting a data_tables row scanned all of
-- data_table_fields.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_data_table_fields_table_id ON data_table_fields (table_id);
