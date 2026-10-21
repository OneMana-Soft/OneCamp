-- no-transaction
-- Foreign key project_forms.created_by -> users (ON DELETE CASCADE) had no
-- index, so deleting a users row scanned all of project_forms.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_project_forms_created_by ON project_forms (created_by);
