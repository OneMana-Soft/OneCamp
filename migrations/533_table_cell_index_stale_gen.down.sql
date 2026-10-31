-- Back to a walk that marks a table's cell index whole whatever happened while
-- it ran.
ALTER TABLE data_tables DROP COLUMN IF EXISTS cell_index_stale_gen;
