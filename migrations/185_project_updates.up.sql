-- Migration 185: a project's updates. Each is where the project stands (its
-- health) and a short note in plain text, posted by one of its admins. The
-- newest is the project's state; the rest are its history. One shared with the
-- client also shows on the project's client link.
CREATE TABLE IF NOT EXISTS project_updates (
    "id"                 uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid"       uuid NOT NULL,
    "author_uuid"        uuid NOT NULL,
    "health"             varchar(16) NOT NULL CHECK (health IN ('on_track', 'at_risk', 'off_track', 'on_hold', 'done')),
    "body"               text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 8000),
    "shared_with_client" boolean NOT NULL DEFAULT false,
    "created_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"         TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS project_updates_project ON project_updates (project_uuid, created_at DESC) WHERE deleted_at IS NULL;
