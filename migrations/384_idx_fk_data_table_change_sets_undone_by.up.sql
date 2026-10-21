-- no-transaction
-- Foreign key data_table_change_sets.undone_by -> data_table_change_sets (ON
-- DELETE SET NULL) had no index, so deleting a data_table_change_sets row
-- scanned all of data_table_change_sets.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_data_table_change_sets_undone_by ON data_table_change_sets (undone_by);
