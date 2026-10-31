-- airtable-03 (tables A4): the Computed Store. A formula's or a rollup's values
-- are kept in data_table_cell_index, typed by what the field gives, so a view
-- sorts, filters, groups and adds up by them along the index, as it does by a
-- stored field (business/RowQuery). A stored value answers a reader only when
-- the reader opens every table it reaches (business/DataTable's gate), and only
-- while it is current: here.

-- One row per formula and rollup, held from the moment it exists: its
-- generation (raised whenever its own settings, or anything in a table its
-- values reach, changes), the generation its entries were filled at and when,
-- the kind of value they hold ('' when the fill found it can't be kept: it
-- reads the clock, the reader's zone or a date, or can't be worked out, and
-- each read works it out), and the tables its values reach (each level of its
-- rollups and linked reads).
CREATE TABLE IF NOT EXISTS data_table_computed (
    field_id   uuid        PRIMARY KEY REFERENCES data_table_fields(id) ON DELETE CASCADE,
    table_id   uuid        NOT NULL,
    gen        bigint      NOT NULL DEFAULT 1,
    filled_gen bigint      NOT NULL DEFAULT 0,
    filled_at  timestamptz,
    kind       text        NOT NULL DEFAULT '',
    reaches    uuid[]      NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_data_table_computed_table ON data_table_computed (table_id);
-- A write in a table finds the stored fields whose values reach it.
CREATE INDEX IF NOT EXISTS idx_data_table_computed_reaches ON data_table_computed USING GIN (reaches);

-- Rows whose stored values wait to be worked out again, marked in the write's
-- own transaction. Working them out deletes only the marks it read (mark at or
-- under the highest it read), so a write landing meanwhile stays marked.
CREATE TABLE IF NOT EXISTS data_table_computed_dirty (
    mark     bigserial   PRIMARY KEY,
    field_id uuid        NOT NULL REFERENCES data_table_computed(field_id) ON DELETE CASCADE,
    row_id   uuid        NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_data_table_computed_dirty_field ON data_table_computed_dirty (field_id, mark);

-- The formulas and rollups there already, for the sweep's first fill.
INSERT INTO data_table_computed (field_id, table_id)
SELECT id, table_id FROM data_table_fields
WHERE type IN ('formula', 'rollup') AND deleted_at IS NULL
ON CONFLICT (field_id) DO NOTHING;

-- Whatever writes a field, in that write's own transaction: a formula or a
-- rollup is held, and a field made, deleted or brought back, its type or
-- settings changed, or made its table's primary one (a link shows its rows by
-- it) raises the generation of every held field of its table and of every
-- held field whose values reach that table. A fill counts only at the
-- generation it started from (MarkComputedFilled), so no path that writes a
-- field, today's or a later one, leaves a kept value current after a change
-- the fill didn't see. A field's name, place or description changes no value
-- and raises nothing. Rows are locked in field order, so two writers never
-- wait on each other.
CREATE OR REPLACE FUNCTION data_table_computed_field_written() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tbl uuid;
BEGIN
    IF TG_OP = 'DELETE' THEN
        tbl := OLD.table_id;
    ELSE
        tbl := NEW.table_id;
        IF NEW.type IN ('formula', 'rollup') THEN
            INSERT INTO data_table_computed (field_id, table_id) VALUES (NEW.id, NEW.table_id)
            ON CONFLICT (field_id) DO NOTHING;
        END IF;
    END IF;
    UPDATE data_table_computed c SET gen = c.gen + 1
    FROM (SELECT field_id FROM data_table_computed
          WHERE table_id = tbl OR reaches @> ARRAY[tbl]
          ORDER BY field_id FOR UPDATE) held
    WHERE c.field_id = held.field_id;
    RETURN NULL;
END
$$;

DROP TRIGGER IF EXISTS data_table_computed_field_made ON data_table_fields;
CREATE TRIGGER data_table_computed_field_made
    AFTER INSERT OR DELETE ON data_table_fields
    FOR EACH ROW EXECUTE FUNCTION data_table_computed_field_written();

DROP TRIGGER IF EXISTS data_table_computed_field_changed ON data_table_fields;
CREATE TRIGGER data_table_computed_field_changed
    AFTER UPDATE ON data_table_fields
    FOR EACH ROW
    WHEN (OLD.type IS DISTINCT FROM NEW.type OR OLD.config IS DISTINCT FROM NEW.config
          OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.is_primary IS DISTINCT FROM NEW.is_primary)
    EXECUTE FUNCTION data_table_computed_field_written();
