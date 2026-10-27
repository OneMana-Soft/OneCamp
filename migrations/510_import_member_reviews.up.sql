-- Who an old Todoist or Notion import made a member of its projects, for an
-- admin to review: before 25 Oct those imports made every person they
-- matched a member of every project they made. A row is one person still in
-- one such project; outcome is what an admin decided ('removed' or 'kept'),
-- NULL until then. Nobody is ever taken off without an admin's say.
CREATE TABLE IF NOT EXISTS import_member_reviews (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "import_id"    uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "provider"     varchar NOT NULL,
    "project_uuid" uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "user_id"      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "outcome"      varchar CHECK (outcome IN ('removed', 'kept')),
    "decided_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "decided_at"   TIMESTAMP WITH TIME ZONE,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT import_member_reviews_one_per_place UNIQUE (project_uuid, user_id)
);

CREATE INDEX IF NOT EXISTS idx_fk_import_member_reviews_import_id ON import_member_reviews (import_id);
CREATE INDEX IF NOT EXISTS idx_fk_import_member_reviews_user_id ON import_member_reviews (user_id);
CREATE INDEX IF NOT EXISTS idx_fk_import_member_reviews_decided_by ON import_member_reviews (decided_by);
