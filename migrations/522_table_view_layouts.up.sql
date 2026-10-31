-- Migration 927 (a placeholder, numbered when it lands): a table's view takes
-- B4's layouts too: a gallery of cards, a list of rows, a timeline of bars
-- across their dates, and a Gantt, the timeline with the links that say what
-- waits on what. Who may make or change a view is unchanged.
ALTER TABLE data_table_views DROP CONSTRAINT IF EXISTS data_table_views_type_check;
ALTER TABLE data_table_views ADD CONSTRAINT data_table_views_type_check
    CHECK (type IN ('grid', 'board', 'calendar', 'chart', 'gallery', 'list', 'timeline', 'gantt'));
