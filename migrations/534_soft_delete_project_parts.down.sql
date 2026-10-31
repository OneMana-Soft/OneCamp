-- The schema before V-78 has no deleted rows of these tables: its deletes
-- removed them. Rows deleted since go as they would have then, so the full
-- unique indexes can be built again.
DELETE FROM task_fields       WHERE deleted_at IS NOT NULL;
DELETE FROM task_statuses     WHERE deleted_at IS NOT NULL;
DELETE FROM project_cycles    WHERE deleted_at IS NOT NULL;
DELETE FROM milestones        WHERE deleted_at IS NOT NULL;
DELETE FROM project_forms     WHERE deleted_at IS NOT NULL;
DELETE FROM task_time_entries WHERE deleted_at IS NOT NULL;
DELETE FROM invoices          WHERE deleted_at IS NOT NULL;

DROP INDEX IF EXISTS invoices_number_idx;
CREATE UNIQUE INDEX IF NOT EXISTS invoices_number_idx ON invoices (lower(number));
DROP INDEX IF EXISTS task_time_entries_one_running;
CREATE UNIQUE INDEX IF NOT EXISTS task_time_entries_one_running ON task_time_entries (user_id) WHERE ended_at IS NULL;
DROP INDEX IF EXISTS task_statuses_project_name_idx;
CREATE UNIQUE INDEX IF NOT EXISTS task_statuses_project_name_idx ON task_statuses (project_id, lower(name));
DROP INDEX IF EXISTS task_fields_project_name_idx;
CREATE UNIQUE INDEX IF NOT EXISTS task_fields_project_name_idx ON task_fields (project_id, lower(name));

ALTER TABLE task_statuses DROP COLUMN IF EXISTS "deleted_moves";
ALTER TABLE task_fields       DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE task_statuses     DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE project_cycles    DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE milestones        DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE project_forms     DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE task_time_entries DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE invoices          DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
