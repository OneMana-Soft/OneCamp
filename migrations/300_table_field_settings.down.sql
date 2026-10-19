-- Migration 300 down: in dependency order. The kinds go back to today's
-- twelve as NOT VALID, so a field made of a newer kind meanwhile doesn't stop
-- the down; the registry still refuses writing one.

DROP INDEX IF EXISTS idx_data_table_rows_row_no;
ALTER TABLE data_table_rows DROP COLUMN IF EXISTS row_no;
ALTER TABLE data_tables     DROP COLUMN IF EXISTS next_row_no;

ALTER TABLE data_table_fields DROP CONSTRAINT IF EXISTS data_table_fields_type_check;
ALTER TABLE data_table_fields ADD CONSTRAINT data_table_fields_type_check CHECK (type IN (
    'text','number','select','multi_select','date','checkbox','person','url','email','relation','formula','rollup'))
    NOT VALID;

DROP INDEX IF EXISTS idx_data_table_fields_primary;
ALTER TABLE data_table_fields DROP COLUMN IF EXISTS is_primary;
ALTER TABLE data_table_fields DROP COLUMN IF EXISTS default_value;
ALTER TABLE data_table_fields DROP COLUMN IF EXISTS required;
ALTER TABLE data_table_fields DROP COLUMN IF EXISTS description;
