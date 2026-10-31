-- Back to migration 535's trigger: any field written raises every kept field of
-- its table and every one reaching it.
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
