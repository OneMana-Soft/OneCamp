-- Migration 186: project templates saved by the workspace. A template is a
-- project's shape (its own statuses and its tasks, with dates counted in days
-- from the project's start) to start new projects from. The built-in ones live
-- in code (business/ProjectTemplate); these are the ones people save, from a
-- project or from a template file.
CREATE TABLE IF NOT EXISTS project_templates (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"        varchar(60) NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    "description" varchar(280) NOT NULL DEFAULT '',
    "body"        jsonb NOT NULL,
    "task_count"  integer NOT NULL DEFAULT 0,
    "preview"     text[] NOT NULL DEFAULT '{}',
    "created_by"  uuid NOT NULL,
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"  TIMESTAMP WITH TIME ZONE
);
CREATE UNIQUE INDEX IF NOT EXISTS project_templates_name ON project_templates (lower(name)) WHERE deleted_at IS NULL;
