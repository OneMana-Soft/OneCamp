-- Migration 930 (a placeholder, numbered when it lands): a table's own forms
-- (tables B5). A form made from a table lives with the table it adds rows to,
-- with no project: project_uuid is NULL and target_table names its table.
-- Every form still has a home: a project, a table, or both (a project's form
-- that adds rows). Postgres 12: every statement can run twice.
ALTER TABLE project_forms ALTER COLUMN project_uuid DROP NOT NULL;
ALTER TABLE project_forms DROP CONSTRAINT IF EXISTS project_forms_home_check;
ALTER TABLE project_forms ADD CONSTRAINT project_forms_home_check
    CHECK (project_uuid IS NOT NULL OR target_table IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_project_forms_table ON project_forms(target_table) WHERE target_table IS NOT NULL;
