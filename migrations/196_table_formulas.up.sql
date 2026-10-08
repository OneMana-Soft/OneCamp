-- Formula fields: worked out from a row's other fields on each read
-- (business/DataTable/formula), so they have a type and a config but no stored
-- cells. The CHECK also gains 'relation', which the code has offered since
-- relation fields were added but this constraint never allowed: creating one
-- failed in the database.
ALTER TABLE data_table_fields DROP CONSTRAINT IF EXISTS data_table_fields_type_check;
ALTER TABLE data_table_fields ADD CONSTRAINT data_table_fields_type_check
    CHECK (type IN ('text','number','select','multi_select','date','checkbox','person','url','email','relation','formula'));
