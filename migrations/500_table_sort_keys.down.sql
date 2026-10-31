-- Back to the row query's index before the sort key. t's indexes come back
-- bounded as the up left them: one over every whole t can't be built while a
-- table holds a long text, so a down that tried would fail where the up
-- succeeded.
ALTER TABLE data_tables DROP COLUMN IF EXISTS cell_index_version;
DROP INDEX IF EXISTS idx_cell_index_s_desc;
DROP INDEX IF EXISTS idx_cell_index_s;
DROP INDEX IF EXISTS idx_cell_index_n_desc;
DROP INDEX IF EXISTS idx_cell_index_n;
CREATE INDEX IF NOT EXISTS idx_cell_index_n ON data_table_cell_index (field_id, n, t, p, row_id) WHERE n IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_n_desc ON data_table_cell_index (field_id, n DESC, t DESC, p, row_id) WHERE n IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_t_desc ON data_table_cell_index (field_id, t DESC, p, row_id)
    WHERE t IS NOT NULL AND octet_length(t) <= 2000;
ALTER TABLE data_table_cell_index DROP COLUMN IF EXISTS s;
