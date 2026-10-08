-- Migration 193: import_errors keeps an item's source id in "source_id", as
-- migration 60 makes it. The import code wrote and read "slack_id", a column
-- the table never had, so every warning and error an import logged was lost.
-- The code now uses source_id; a database whose table still has the old
-- column from before migration 60 has it renamed.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'import_errors' AND column_name = 'slack_id')
       AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'import_errors' AND column_name = 'source_id') THEN
        ALTER TABLE import_errors RENAME COLUMN slack_id TO source_id;
    END IF;
END $$;
