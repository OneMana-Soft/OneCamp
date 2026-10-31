-- no-transaction
-- A form's logo: project_forms.logo_id references attachments(id) ON DELETE
-- SET NULL, and nothing indexed it, so every attachment delete scanned
-- project_forms for logos to clear. Partial: only a form with a logo holds
-- one, and the delete looks for that one id. The cover has its own migration
-- beside this one, since a file holds one statement.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_project_forms_logo_id ON project_forms (logo_id) WHERE logo_id IS NOT NULL;
