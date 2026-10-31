-- Migration 472, a placeholder the integrator numbers at landing, after the
-- row query's (tables T5c, design/tables-vs-airtable/PROGRAM.md §14): the
-- cell index's natural sort key, and a long text that can be saved.
--
-- s is a text's, or a choice's label's, natural sort key (business/RowQuery
-- naturalKey, and the browser's lib/tables/sortKey.ts, both held to
-- testdata/sortkey/natural.json): "Row 2" before "Row 10", case and accents
-- folded, cut to 256 code points. Sorts read s; filters read t, the text
-- itself, lower-cased and trimmed.
--
-- t's btree indexes held each text whole, and Postgres refuses a btree entry
-- over 2,704 bytes: the save of a long text failed (3,200 bytes that don't
-- compress, or about 12,000 characters of prose), and so did the repair
-- sweep's every pass over its table. t's sort index goes, as sorts read s;
-- its equality index keeps the texts it can hold, up to 2,000 bytes
-- (models/postgres/DataTable IndexedTextBytes and ShortText say the same
-- words, so the planner reads it). A filter still reads the whole t, from
-- each row's own entry.
--
-- A choice sorts by its option's place, then its label's key: its walks read
-- (n, s), so the n indexes move from (n, t) to (n, s). A number's s is NULL,
-- so n alone orders it, as before.
ALTER TABLE data_table_cell_index ADD COLUMN IF NOT EXISTS s text COLLATE "C";

DROP INDEX IF EXISTS idx_cell_index_t_desc;
DROP INDEX IF EXISTS idx_cell_index_t;
CREATE INDEX IF NOT EXISTS idx_cell_index_t ON data_table_cell_index (field_id, t, p, row_id)
    WHERE t IS NOT NULL AND octet_length(t) <= 2000;

DROP INDEX IF EXISTS idx_cell_index_n;
DROP INDEX IF EXISTS idx_cell_index_n_desc;
CREATE INDEX IF NOT EXISTS idx_cell_index_n ON data_table_cell_index (field_id, n, s, p, row_id) WHERE n IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_n_desc ON data_table_cell_index (field_id, n DESC, s DESC, p, row_id) WHERE n IS NOT NULL;

-- A sort on a text, both ways (a tie keeps the table's order either way).
CREATE INDEX IF NOT EXISTS idx_cell_index_s ON data_table_cell_index (field_id, s, p, row_id) WHERE s IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cell_index_s_desc ON data_table_cell_index (field_id, s DESC, p, row_id) WHERE s IS NOT NULL;

-- What a table's cell index was made whole with (business/RowQuery
-- IndexVersion): 1 before s. The repair sweep makes a table of an older
-- version whole again, a batch of rows at a time; until then its sorts on a
-- text or a choice are answered by the evaluator, and its bundle tells the
-- browser to keep them.
ALTER TABLE data_tables ADD COLUMN IF NOT EXISTS cell_index_version smallint NOT NULL DEFAULT 1;
