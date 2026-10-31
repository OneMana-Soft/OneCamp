-- Undoes 930: a table's own forms go (their answers' rows stay in the table),
-- and every form needs a project again.
DROP INDEX IF EXISTS idx_project_forms_table;
DELETE FROM project_forms WHERE project_uuid IS NULL;
ALTER TABLE project_forms DROP CONSTRAINT IF EXISTS project_forms_home_check;
ALTER TABLE project_forms ALTER COLUMN project_uuid SET NOT NULL;
