-- Migration 470, a placeholder the integrator numbers at landing (302 in its programme range, then 357; tables A3, design/tables-vs-airtable/PROGRAM.md §3.7): Row
-- Query. The server answers every question about a table's rows
-- (business/RowQuery).
--
-- The cell index (D-TB-1): each stored cell's value typed for comparing and
-- sorting, kept by the row ledger in the same transaction as the row, so a
-- filter or a sort on any field reads an index rather than every row's JSON
-- (PM-5: a select filter took 6.5 s at 100,000 rows). n is a number (a
-- choice's option's place), t text as it sorts (trimmed, lower case, byte
-- order; a choice's label), d a moment (a day's midnight at UTC, as it
-- sorts), b a tick, a a list's labels or ids,
-- and p the row's place (its position_key), so a sort on a field walks one
-- index in (value, place, row) order, ties in the table's order, and stops
-- at a window's end. A row's cells in the trash stay until the row is gone
-- for good.
CREATE TABLE IF NOT EXISTS data_table_cell_index (
    table_id uuid        NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    field_id uuid        NOT NULL REFERENCES data_table_fields(id) ON DELETE CASCADE,
    row_id   uuid        NOT NULL REFERENCES data_table_rows(id) ON DELETE CASCADE,
    n        numeric,
    t        text COLLATE "C",
    d        timestamptz,
    b        boolean,
    a        text[],
    p        text COLLATE "C",
    PRIMARY KEY (row_id, field_id)
);
-- Each sort's walk, both ways (a tie keeps the table's order either way).
-- A field's id is unique across tables, so the field leads: a probe for one
-- row's cell reads the primary key, and a walk or a set reads the field's own
-- range. table_id is for a table's removal.
CREATE INDEX IF NOT EXISTS idx_cell_index_n ON data_table_cell_index (field_id, n, t, p, row_id) WHERE n IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_n_desc ON data_table_cell_index (field_id, n DESC, t DESC, p, row_id) WHERE n IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_t ON data_table_cell_index (field_id, t, p, row_id) WHERE t IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_t_desc ON data_table_cell_index (field_id, t DESC, p, row_id) WHERE t IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_d ON data_table_cell_index (field_id, d, p, row_id) WHERE d IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_d_desc ON data_table_cell_index (field_id, d DESC, p, row_id) WHERE d IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_b ON data_table_cell_index (field_id, b, p, row_id) WHERE b IS NOT NULL;
-- A walked field's empty cells (a text, a number, a choice, a date, a tick
-- keeps an entry with no value for each), in the table's order: the rows
-- with no value, after a sort's values.
CREATE INDEX IF NOT EXISTS idx_cell_index_empty ON data_table_cell_index (field_id, p, row_id)
    WHERE n IS NULL AND t IS NULL AND d IS NULL AND b IS NULL AND a IS NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_table ON data_table_cell_index (table_id);
-- A column's delete cascades here by field_id alone, which none of the
-- partial indexes above can serve (TestEveryForeignKeyHasAnIndex): without
-- this, deleting one column read every table's cells.
CREATE INDEX IF NOT EXISTS idx_cell_index_field ON data_table_cell_index (field_id);
CREATE INDEX IF NOT EXISTS idx_cell_index_a ON data_table_cell_index USING GIN (a) WHERE a IS NOT NULL;

-- When a table's cell index was last made whole (tables.cellindex.repair):
-- a query reads the index only for a table that has one; until then it is
-- answered by Row Query's evaluator over the rows one read can work out.
ALTER TABLE data_tables ADD COLUMN IF NOT EXISTS cell_index_at timestamptz;
CREATE INDEX IF NOT EXISTS idx_data_tables_unindexed ON data_tables (created_at) WHERE cell_index_at IS NULL AND deleted_at IS NULL;
