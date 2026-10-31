-- airtable-03b (V-102): how far a stored formula's or rollup's whole fill has
-- got, so a read past what one read works out says so ("Double is being worked
-- out over 100,000 rows: 42% done") and asks again, where it was refused. A
-- fill sets fill_total to the table's rows and fill_done to 0 as it starts,
-- and each batch adds its rows to fill_done in the batch's own transaction.
ALTER TABLE data_table_computed
    ADD COLUMN IF NOT EXISTS fill_done  bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS fill_total bigint NOT NULL DEFAULT 0;
