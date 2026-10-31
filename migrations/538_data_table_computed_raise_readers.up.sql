-- V-108 (main, 30 Oct): migration 535's trigger raised the generation of every
-- kept formula and rollup of a table, and of every one reaching it, whenever
-- any field of that table was made, deleted or changed, so the sweep filled
-- each of them again over every row: about 100 s at 100,000 rows for a new
-- column none of them reads. A field written now raises only itself and the
-- kept fields that read it, at every level.
--
-- A formula or rollup reads a field when its settings name the field's id: a
-- formula is stored with each {Field} written as its id (and {Link}.{Field}
-- as both), and a rollup names its link, the field it adds up and the fields
-- it filters and sorts by. One that reads a raised field is raised too, on
-- any table. A field made has an id no setting names yet, so it raises
-- nothing (a formula or rollup made is held, unfilled, and the sweep fills
-- it). A table's primary field is what its rows show as through a link, so
-- a write to the primary one, or a move of it, also raises every kept field
-- reaching the table, as before. A field's name, place or description changes
-- no value and raises nothing (the trigger's WHEN). Rows are locked in field
-- order, so two writers never wait on each other.
CREATE OR REPLACE FUNCTION data_table_computed_field_written() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tbl   uuid;
    fld   uuid;
    shown boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        tbl := OLD.table_id;
        fld := OLD.id;
        shown := OLD.is_primary;
    ELSE
        tbl := NEW.table_id;
        fld := NEW.id;
        shown := NEW.is_primary OR (TG_OP = 'UPDATE' AND OLD.is_primary);
        IF NEW.type IN ('formula', 'rollup') THEN
            INSERT INTO data_table_computed (field_id, table_id) VALUES (NEW.id, NEW.table_id)
            ON CONFLICT (field_id) DO NOTHING;
        END IF;
    END IF;
    IF TG_OP = 'INSERT' AND NOT shown THEN
        RETURN NULL;
    END IF;
    WITH RECURSIVE readers(id) AS (
        SELECT r FROM unnest(ARRAY[fld] || ARRAY(
            SELECT h.field_id FROM data_table_computed h WHERE shown AND h.reaches @> ARRAY[tbl])) AS r
        UNION
        SELECT h.field_id
        FROM readers w
        JOIN data_table_fields f ON strpos(f.config::text, w.id::text) > 0
        JOIN data_table_computed h ON h.field_id = f.id
    )
    UPDATE data_table_computed c SET gen = c.gen + 1
    FROM (SELECT field_id FROM data_table_computed
          WHERE field_id IN (SELECT id FROM readers)
          ORDER BY field_id FOR UPDATE) held
    WHERE c.field_id = held.field_id;
    RETURN NULL;
END
$$;
