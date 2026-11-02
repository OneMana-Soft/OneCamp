-- The schema before V-184 head 3 has no taken-away roles: its deletes removed
-- them. Rows marked since go as they would have then, first, so no role taken
-- away comes back live when the columns go.
DELETE FROM project_roles WHERE deleted_at IS NOT NULL;
ALTER TABLE project_roles DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
