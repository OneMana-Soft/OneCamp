-- V-78 (the owner: "all the deletes were soft delete"): a project's custom
-- fields, statuses, cycles, milestones and forms (a table's forms too), a
-- person's time entries and a draft invoice are deleted softly. The row stays,
-- marked with when and by whom, so a restore (POST /restore/{kind}/{id}, the
-- Undo right after a delete) brings it back with what hangs off it: a field's
-- task values, a cycle's or milestone's tasks, a form's submissions. Every read
-- of these tables takes live rows only (models/postgres/SoftDelete.Live).
ALTER TABLE task_fields       ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE task_statuses     ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE project_cycles    ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE milestones        ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE project_forms     ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE task_time_entries ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE invoices          ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;

-- A deleted status moved its tasks to another one: which tasks, and where, so
-- its restore moves back those that are still there.
ALTER TABLE task_statuses ADD COLUMN IF NOT EXISTS "deleted_moves" jsonb;

-- A name is taken among a project's live fields and statuses only: a deleted
-- "Budget" leaves the name free, and restoring it while a new "Budget" lives is
-- refused in words (the restore's unique violation).
DROP INDEX IF EXISTS task_fields_project_name_idx;
CREATE UNIQUE INDEX IF NOT EXISTS task_fields_project_name_idx ON task_fields (project_id, lower(name)) WHERE deleted_at IS NULL;
DROP INDEX IF EXISTS task_statuses_project_name_idx;
CREATE UNIQUE INDEX IF NOT EXISTS task_statuses_project_name_idx ON task_statuses (project_id, lower(name)) WHERE deleted_at IS NULL;

-- A deleted draft's number is the next again, as when its delete was hard:
-- numbers are unique among live invoices. Its restore takes the next free
-- number when another invoice has taken its own since.
DROP INDEX IF EXISTS invoices_number_idx;
CREATE UNIQUE INDEX IF NOT EXISTS invoices_number_idx ON invoices (lower(number)) WHERE deleted_at IS NULL;

-- One running timer per person among live entries: a deleted running entry
-- doesn't stop the next one starting.
DROP INDEX IF EXISTS task_time_entries_one_running;
CREATE UNIQUE INDEX IF NOT EXISTS task_time_entries_one_running ON task_time_entries (user_id) WHERE ended_at IS NULL AND deleted_at IS NULL;
