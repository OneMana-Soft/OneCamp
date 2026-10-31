DROP INDEX IF EXISTS idx_fk_data_table_views_owner_id;

-- Views of a person's own go with the column that says whose they are, and
-- charts with the layout the older check doesn't know.
DELETE FROM data_table_views WHERE owner_id IS NOT NULL OR type = 'chart';

ALTER TABLE data_table_views DROP CONSTRAINT IF EXISTS data_table_views_type_check;
ALTER TABLE data_table_views ADD CONSTRAINT data_table_views_type_check
    CHECK (type IN ('grid', 'board', 'calendar'));

ALTER TABLE data_table_views
    DROP COLUMN IF EXISTS locked,
    DROP COLUMN IF EXISTS owner_id;
