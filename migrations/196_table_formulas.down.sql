-- Fields of the two kinds this takes away become text first, or the old
-- constraint couldn't be put back.
UPDATE data_table_fields SET type = 'text' WHERE type IN ('relation','formula');
ALTER TABLE data_table_fields DROP CONSTRAINT IF EXISTS data_table_fields_type_check;
ALTER TABLE data_table_fields ADD CONSTRAINT data_table_fields_type_check
    CHECK (type IN ('text','number','select','multi_select','date','checkbox','person','url','email'));
