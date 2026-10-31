-- Migration 473, a placeholder the integrator numbers at landing, after the
-- cell index's sort keys (tables T5c, design/tables-vs-airtable/PROGRAM.md
-- §14): a table the repair sweep can't index no longer holds up the tables
-- made after it.
--
-- tables.cellindex.repair takes the tables whose cell index isn't whole,
-- oldest first. One whose indexing failed (a long text's did, before 472) was
-- the first again on every pass, and every pass stopped at it, so no table
-- made after it was indexed, and its error was logged every five minutes.
--
-- Now the sweep records the failure on the table and goes on to the next one:
-- cell_index_failures is how many passes in a row couldn't index it, and
-- cell_index_retry_at is when a pass may try it again, five minutes after the
-- first failure and twice as long after each one after it, up to a day
-- (business/RowQuery indexTables, through helpers.Backoff). Its error is
-- logged at the first failure. Making the index whole clears both.
ALTER TABLE data_tables ADD COLUMN IF NOT EXISTS cell_index_failures integer NOT NULL DEFAULT 0;
ALTER TABLE data_tables ADD COLUMN IF NOT EXISTS cell_index_retry_at timestamptz;
