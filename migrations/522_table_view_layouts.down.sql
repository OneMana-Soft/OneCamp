-- Views in B4's layouts go with the layouts the older check knows.
DELETE FROM data_table_views WHERE type IN ('gallery', 'list', 'timeline', 'gantt');

ALTER TABLE data_table_views DROP CONSTRAINT IF EXISTS data_table_views_type_check;
ALTER TABLE data_table_views ADD CONSTRAINT data_table_views_type_check
    CHECK (type IN ('grid', 'board', 'calendar', 'chart'));
