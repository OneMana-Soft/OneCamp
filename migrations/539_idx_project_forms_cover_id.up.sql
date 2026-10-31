-- no-transaction
-- A form's cover: project_forms.cover_id references attachments(id) ON DELETE
-- SET NULL, and nothing indexed it, so every attachment delete scanned
-- project_forms for covers to clear. Partial: only a form with a cover holds
-- one, and the delete looks for that one id. The logo has its own migration
-- beside this one, since a file holds one statement.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_project_forms_cover_id ON project_forms (cover_id) WHERE cover_id IS NOT NULL;
