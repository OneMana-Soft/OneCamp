-- Tables S1 step 5 (design/tables-vs-airtable/PROGRAM.md §15): an operation
-- is several change sets a person undoes as one, as an import of more rows
-- than one change set holds (Batch writes 1,000 at most). An undo of any of
-- them reverses them all, its own change sets sharing an operation of their
-- own, which a redo reverses together. The number is a placeholder: the
-- integrator numbers it.
ALTER TABLE data_table_change_sets ADD COLUMN IF NOT EXISTS operation uuid;

-- A table's change sets of one operation, for an undo of it.
CREATE INDEX IF NOT EXISTS idx_data_table_change_sets_operation
    ON data_table_change_sets (table_id, operation) WHERE operation IS NOT NULL;
