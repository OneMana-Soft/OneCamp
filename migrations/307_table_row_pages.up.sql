-- Migration 307: a row's page (tables T2, rows as pages; docs D-18).
--
-- A row can be opened as a page: a doc of its own under its fields, made the
-- first time someone writes in it. The doc names its host in the graph
-- (doc_host_kind "table_row", doc_host_uuid the row's id), and its level is
-- the row's; this is the row's side of the link, so a table's rows say which
-- have a page without asking the graph, and a table's pages are found when who
-- may see the table changes. One page a row, and a doc is one row's page.

ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS doc_uuid uuid;

CREATE UNIQUE INDEX IF NOT EXISTS idx_data_table_rows_doc
    ON data_table_rows (doc_uuid) WHERE doc_uuid IS NOT NULL;
