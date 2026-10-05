-- Migration 181: time on tasks. A person runs a timer on a task or adds time
-- by hand; a project's members see the totals by person and by task, and
-- export them for invoicing. ended_at NULL is a running timer, and a person
-- has at most one.
CREATE TABLE IF NOT EXISTS task_time_entries (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "task_uuid"    uuid NOT NULL,
    "project_uuid" uuid NOT NULL,
    "user_id"      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "started_at"   TIMESTAMP WITH TIME ZONE NOT NULL,
    "ended_at"     TIMESTAMP WITH TIME ZONE,
    "note"         varchar(500) NOT NULL DEFAULT '',
    "billable"     boolean NOT NULL DEFAULT true,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CHECK (ended_at IS NULL OR ended_at > started_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS task_time_entries_one_running ON task_time_entries (user_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS task_time_entries_project ON task_time_entries (project_uuid, started_at);
CREATE INDEX IF NOT EXISTS task_time_entries_task ON task_time_entries (task_uuid, started_at);
