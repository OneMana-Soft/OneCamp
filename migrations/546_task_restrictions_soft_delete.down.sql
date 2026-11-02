-- The schema before V-179 has no taken-off rows: its deletes removed them.
-- Rows marked since go as they would have then (a lifted restriction's grants
-- and the tasks restricted with it went with it, by cascade).
DELETE FROM task_restriction_grants WHERE deleted_at IS NOT NULL;
DELETE FROM task_restrictions WHERE deleted_at IS NOT NULL;

ALTER TABLE task_restriction_grants DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE task_restrictions       DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
