-- Back to a repair sweep that takes every table whose cell index isn't whole,
-- oldest first, on every pass, without a record of the ones that failed.
ALTER TABLE data_tables DROP COLUMN IF EXISTS cell_index_retry_at;
ALTER TABLE data_tables DROP COLUMN IF EXISTS cell_index_failures;
