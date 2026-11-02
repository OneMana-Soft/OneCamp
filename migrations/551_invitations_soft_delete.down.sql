-- The schema before V-184 head 3c has no withdrawn invitations: its
-- withdrawal removed them. Rows marked since go as they would have then,
-- first, so no withdrawn invitation comes back live when the columns go.
DELETE FROM invitations WHERE deleted_at IS NOT NULL;
ALTER TABLE invitations DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
