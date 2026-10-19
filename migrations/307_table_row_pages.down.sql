-- Migration 307 down: rows forget their pages. The docs stay in the graph,
-- each still naming its row, and open to nobody but through the row's page,
-- which a build with 307 again finds by the row.

DROP INDEX IF EXISTS idx_data_table_rows_doc;
ALTER TABLE data_table_rows DROP COLUMN IF EXISTS doc_uuid;
