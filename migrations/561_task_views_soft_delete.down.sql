-- The views deleted go before their marks: 564's down has taken them already
-- (it puts the full unique back), so this finds none unless it never ran.
DELETE FROM task_views WHERE deleted_at IS NOT NULL;
ALTER TABLE task_views DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
