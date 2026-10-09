-- Rollup fields: a value worked out on each read from the rows a relation
-- field links to (business/DataTable/computed.go), so, like formulas, they
-- have a type and a config but no stored cells.
ALTER TABLE data_table_fields DROP CONSTRAINT IF EXISTS data_table_fields_type_check;
ALTER TABLE data_table_fields ADD CONSTRAINT data_table_fields_type_check
    CHECK (type IN ('text','number','select','multi_select','date','checkbox','person','url','email','relation','formula','rollup'));

-- The links a relation field makes between tables' rows, one row each: from
-- a row of the field's table to a row of the table it links to, in the order
-- they were made. Found from either end by index, so a table's rows show the
-- rows linking to them without reading the linking table through; kept out of
-- the rows' values, so saving a row's other cells never touches its links.
CREATE TABLE IF NOT EXISTS data_table_links (
    field_id   uuid        NOT NULL REFERENCES data_table_fields(id) ON DELETE CASCADE,
    from_row   uuid        NOT NULL REFERENCES data_table_rows(id) ON DELETE CASCADE,
    to_row     uuid        NOT NULL REFERENCES data_table_rows(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (field_id, from_row, to_row)
);
-- A row's links, and the rows linking to it, in the order they were made:
-- read a page at a time, by index, however many a row has.
CREATE INDEX IF NOT EXISTS idx_data_table_links_from ON data_table_links (field_id, from_row, created_at, to_row);
CREATE INDEX IF NOT EXISTS idx_data_table_links_to ON data_table_links (field_id, to_row, created_at, from_row);
-- A deleted row's links, from either end, go with it.
CREATE INDEX IF NOT EXISTS idx_data_table_links_from_row ON data_table_links (from_row);
CREATE INDEX IF NOT EXISTS idx_data_table_links_to_row ON data_table_links (to_row);
