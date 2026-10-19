-- Migration 301 down: the row ledger goes, with every change set and change
-- it recorded; rows keep their cells, places by number and deletes.

DROP TABLE IF EXISTS data_table_changes;
DROP TABLE IF EXISTS data_table_change_sets;

DROP INDEX IF EXISTS idx_data_table_fields_trash;
ALTER TABLE data_table_fields DROP COLUMN IF EXISTS deleted_by;
ALTER TABLE data_tables       DROP COLUMN IF EXISTS deleted_by;

DROP INDEX IF EXISTS idx_data_table_rows_unkeyed;
DROP INDEX IF EXISTS idx_data_table_rows_trash;
DROP INDEX IF EXISTS idx_data_table_rows_order;

ALTER TABLE data_table_rows DROP COLUMN IF EXISTS position_key;
ALTER TABLE data_table_rows DROP COLUMN IF EXISTS deleted_by;
ALTER TABLE data_table_rows DROP COLUMN IF EXISTS updated_by;
ALTER TABLE data_table_rows DROP COLUMN IF EXISTS version;
