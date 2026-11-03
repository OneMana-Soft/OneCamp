-- V-170 (tables, 1 Nov 2026): who edits a table's rows. Until now everyone who
-- could open a workspace table could change it. A table now keeps a general
-- level for its members and the people granted edit beyond it
-- (business/DataTable editAccess.go):
--   general_level 'edit'    everyone who can open it edits its rows (the
--                           default, and every table's level before this);
--   general_level 'comment' only its managers and the people granted edit
--                           do; the rest read, export and comment;
--   general_level 'view'    kept for the plain viewer the sharing head adds.
-- Who may OPEN a table is unchanged: private is its creator and admins,
-- workspace is every member, a guest link reads.
ALTER TABLE data_tables ADD COLUMN IF NOT EXISTS general_level text NOT NULL DEFAULT 'edit'
    CHECK (general_level IN ('view', 'comment', 'edit'));

-- The people granted edit on a table. Removing a grant is a soft delete
-- (deleted_at, deleted_by): reads see only live grants, and one live grant per
-- person and table, so a person granted again gets a new row and the old one
-- stays as it was.
CREATE TABLE IF NOT EXISTS data_table_grants (
    id         bigserial PRIMARY KEY,
    table_id   uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    level      text NOT NULL DEFAULT 'edit' CHECK (level = 'edit'),
    granted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    granted_at timestamptz NOT NULL DEFAULT NOW(),
    deleted_at timestamptz,
    deleted_by uuid REFERENCES users(id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS data_table_grants_live ON data_table_grants (table_id, user_id) WHERE deleted_at IS NULL;
-- Every foreign key has an index leading with it (deleting a table or a user
-- looks here).
CREATE INDEX IF NOT EXISTS idx_fk_data_table_grants_table_id ON data_table_grants (table_id);
CREATE INDEX IF NOT EXISTS idx_fk_data_table_grants_user_id ON data_table_grants (user_id);
CREATE INDEX IF NOT EXISTS idx_fk_data_table_grants_granted_by ON data_table_grants (granted_by) WHERE granted_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_fk_data_table_grants_deleted_by ON data_table_grants (deleted_by) WHERE deleted_by IS NOT NULL;
