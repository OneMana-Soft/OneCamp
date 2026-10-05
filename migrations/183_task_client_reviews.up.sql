-- Migration 183: a client's verdict on a task, given from a project link: they
-- approve it or ask for changes. Each decision is kept (the newest is the
-- task's state), and is also posted as a comment so the team is told the way
-- any comment tells them.
CREATE TABLE IF NOT EXISTS task_client_reviews (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "task_uuid"    uuid NOT NULL,
    "project_uuid" uuid NOT NULL,
    "grant_id"     uuid NOT NULL REFERENCES guest_grants(id) ON DELETE CASCADE,
    "decision"     varchar(16) NOT NULL CHECK (decision IN ('approved', 'changes')),
    "name"         varchar(60) NOT NULL,
    "note"         varchar(2000) NOT NULL DEFAULT '',
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS task_client_reviews_task ON task_client_reviews (task_uuid, created_at DESC);
CREATE INDEX IF NOT EXISTS task_client_reviews_project ON task_client_reviews (project_uuid, created_at DESC);
