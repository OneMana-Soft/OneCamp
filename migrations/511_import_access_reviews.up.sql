-- What old Jira, Linear and Basecamp imports brought in that their source
-- keeps to some people: before each importer's fix (its key in
-- business/Import importFixes), an import let every member of the project read
-- it. A review is one import's project and one gap, re-checked against the
-- source where the import's own connection still works, else an admin's to
-- decide. state is where it stands, summary its counts. Nothing here opens
-- anything.
CREATE TABLE IF NOT EXISTS import_access_reviews (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "import_id"    uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "provider"     varchar NOT NULL,
    "project_uuid" uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "gap"          varchar NOT NULL
        CHECK (gap IN ('jira_security', 'jira_comments', 'linear_teams', 'basecamp_clients')),
    "state"        varchar NOT NULL
        CHECK (state IN ('rechecking', 'rechecked', 'needs_admin', 'kept', 'contained', 'undone')),
    "summary"      jsonb NOT NULL DEFAULT '{}',
    "decided_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "decided_at"   TIMESTAMP WITH TIME ZONE,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT import_access_reviews_one_per_gap UNIQUE (import_id, project_uuid, gap)
);

CREATE INDEX IF NOT EXISTS idx_fk_import_access_reviews_project_uuid ON import_access_reviews (project_uuid);
CREATE INDEX IF NOT EXISTS idx_fk_import_access_reviews_decided_by ON import_access_reviews (decided_by);

-- Each change a re-check made, with what was there before, so an admin can
-- undo one import's re-check: a task restricted (after: who it let in), an
-- assignee taken off (before: who), a comment hidden. undone_at is when it
-- was put back.
CREATE TABLE IF NOT EXISTS import_access_changes (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "review_id"   uuid NOT NULL REFERENCES import_access_reviews(id) ON DELETE CASCADE,
    "kind"        varchar NOT NULL CHECK (kind IN ('restricted', 'unassigned', 'comment_hidden')),
    "entity_uuid" uuid NOT NULL,
    "before"      jsonb,
    "after"       jsonb,
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "undone_at"   TIMESTAMP WITH TIME ZONE,
    CONSTRAINT import_access_changes_once UNIQUE (review_id, kind, entity_uuid)
);

CREATE INDEX IF NOT EXISTS idx_import_access_changes_entity_uuid ON import_access_changes (entity_uuid);

-- One import's comments by their task's source id, as a re-check reads
-- each task's comments.
CREATE INDEX IF NOT EXISTS idx_import_id_map_parent ON import_id_map (import_id, entity_type, parent_source_id);

-- Why a person is on migration 510's list: 'basecamp_client' for someone an
-- old Basecamp import made a member who is a client there; NULL for the
-- people old Todoist and Notion imports added.
ALTER TABLE import_member_reviews ADD COLUMN IF NOT EXISTS "reason" varchar
    CONSTRAINT import_member_reviews_reason_check CHECK (reason IN ('basecamp_client'));
