-- The kept formula and rollup values go first: code from before this
-- migration never wrote one into the cell index, so none stays behind.
DELETE FROM data_table_cell_index
WHERE field_id IN (SELECT id FROM data_table_fields WHERE type IN ('formula', 'rollup'));
DROP TRIGGER IF EXISTS data_table_computed_field_changed ON data_table_fields;
DROP TRIGGER IF EXISTS data_table_computed_field_made ON data_table_fields;
DROP FUNCTION IF EXISTS data_table_computed_field_written();
DROP TABLE IF EXISTS data_table_computed_dirty;
DROP TABLE IF EXISTS data_table_computed;
