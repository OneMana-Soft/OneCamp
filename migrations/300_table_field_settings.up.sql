-- Migration 300 (tables A1, design/tables-vs-airtable/PROGRAM.md §3.7): what a
-- field says about its cells, and a number for every row.
--
-- A field gains a description (shown in its header and the record), a default
-- value for new rows, "required" (enforced where a whole row is written: the
-- API, AI tools and forms), and which field is the table's primary one, the
-- field that names its rows. Each table's primary is the field the web has
-- always shown first and kept from being hidden: its first by position.
--
-- The kinds a field may be widen to the ones the programme builds (D-TB-3);
-- the server's field-type registry (business/FieldTypes) refuses each until
-- it is registered, so no field of a kind nothing can read is ever made.
--
-- Every row gets a number in its table, in the order the rows were made
-- (deleted rows too, so a number is never given twice), for autonumber fields
-- and short links to a row; a table counts on from its highest.

ALTER TABLE data_table_fields ADD COLUMN IF NOT EXISTS description   text;
ALTER TABLE data_table_fields ADD COLUMN IF NOT EXISTS required      boolean NOT NULL DEFAULT false;
ALTER TABLE data_table_fields ADD COLUMN IF NOT EXISTS default_value jsonb;
ALTER TABLE data_table_fields ADD COLUMN IF NOT EXISTS is_primary    boolean NOT NULL DEFAULT false;

UPDATE data_table_fields f SET is_primary = true
FROM (
    SELECT DISTINCT ON (table_id) id, table_id FROM data_table_fields
    WHERE deleted_at IS NULL
    ORDER BY table_id, position, created_at, id
) p
WHERE f.id = p.id
  AND NOT EXISTS (SELECT 1 FROM data_table_fields g
                  WHERE g.table_id = p.table_id AND g.is_primary AND g.deleted_at IS NULL);

CREATE UNIQUE INDEX IF NOT EXISTS idx_data_table_fields_primary
    ON data_table_fields (table_id) WHERE is_primary AND deleted_at IS NULL;

ALTER TABLE data_table_fields DROP CONSTRAINT IF EXISTS data_table_fields_type_check;
ALTER TABLE data_table_fields ADD CONSTRAINT data_table_fields_type_check CHECK (type IN (
    'text','number','select','multi_select','date','checkbox','person','url','email','relation','formula','rollup',
    'long_text','currency','percent','duration','rating','people','attachment','phone','barcode','button',
    'autonumber','created_time','created_by','modified_time','modified_by','lookup','count','status','ai'));

ALTER TABLE data_tables     ADD COLUMN IF NOT EXISTS next_row_no bigint NOT NULL DEFAULT 1;
ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS row_no      bigint;

UPDATE data_table_rows r SET row_no = n.no
FROM (
    SELECT id, row_number() OVER (PARTITION BY table_id ORDER BY created_at, position, id) AS no
    FROM data_table_rows
) n
WHERE r.id = n.id AND r.row_no IS NULL;

UPDATE data_tables t SET next_row_no = COALESCE(
    (SELECT MAX(row_no) FROM data_table_rows r WHERE r.table_id = t.id), 0) + 1;

CREATE UNIQUE INDEX IF NOT EXISTS idx_data_table_rows_row_no ON data_table_rows (table_id, row_no);
