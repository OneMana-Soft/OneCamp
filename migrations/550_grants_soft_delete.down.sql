-- The schema before V-184 head 3b has no stopped shares and no taken admin
-- roles: its deletes removed them. Rows marked since go as they would have
-- then, first, so no share stopped and no admin role taken away comes back
-- live when the columns go.
DELETE FROM calendar_shares WHERE deleted_at IS NOT NULL;
ALTER TABLE calendar_shares DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
DELETE FROM admin_users WHERE deleted_at IS NOT NULL;
ALTER TABLE admin_users DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
-- The first-admin setup mark (system_configs 'first_admin_setup') stays: the
-- schema before this migration never reads it, and a workspace set up stays
-- set up when it comes back up.
