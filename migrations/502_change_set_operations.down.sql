DROP INDEX IF EXISTS idx_data_table_change_sets_operation;
ALTER TABLE data_table_change_sets DROP COLUMN IF EXISTS operation;
