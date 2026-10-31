-- Migration 512: a table's saved views as a person's own or shared, and
-- shared ones locked. A view with an owner is that person's alone: only they
-- list, change or delete it. One with none is shared with everyone who reads
-- the table, and its managers make, change and delete it; a locked one takes
-- no change until a manager unlocks it. A chart is a view's layout too, as
-- the grid draws it and an import brings it.
ALTER TABLE data_table_views
    ADD COLUMN IF NOT EXISTS owner_id uuid REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS locked boolean NOT NULL DEFAULT false;

ALTER TABLE data_table_views DROP CONSTRAINT IF EXISTS data_table_views_type_check;
ALTER TABLE data_table_views ADD CONSTRAINT data_table_views_type_check
    CHECK (type IN ('grid', 'board', 'calendar', 'chart'));

-- A person's deletion finds their views by it (the foreign key's index).
CREATE INDEX IF NOT EXISTS idx_fk_data_table_views_owner_id
    ON data_table_views (owner_id)
    WHERE owner_id IS NOT NULL;
