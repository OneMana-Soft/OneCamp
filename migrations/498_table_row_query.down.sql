DROP INDEX IF EXISTS idx_cell_index_field;
DROP INDEX IF EXISTS idx_data_tables_unindexed;
ALTER TABLE data_tables DROP COLUMN IF EXISTS cell_index_at;
DROP TABLE IF EXISTS data_table_cell_index;
