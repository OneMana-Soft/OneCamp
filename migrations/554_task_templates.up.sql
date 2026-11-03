-- Migration 554: task templates (linear-09). A template is one task's shape
-- (business/TaskTemplate: its name, body, status, priority, tags, days,
-- sub-tasks, type, assignee, field values and the properties its maker must
-- fill), kept for a project by its admins or, with no project, for the whole
-- workspace by its admins. Deleting one is soft (deleted_at, deleted_by), and
-- while it is deleted its name and the type it is the default for are free.
CREATE TABLE IF NOT EXISTS task_templates (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_id"  uuid,
    "name"        varchar(60) NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    "description" varchar(280) NOT NULL DEFAULT '',
    "body"        jsonb NOT NULL,
    "default_for" uuid,
    "created_by"  uuid NOT NULL,
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_by"  uuid,
    "updated_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"  TIMESTAMP WITH TIME ZONE,
    "deleted_by"  uuid
);
-- A name is taken once in a project, and once in the workspace, among the
-- live templates. The scope comes first, so a project's list reads this index.
CREATE UNIQUE INDEX IF NOT EXISTS task_templates_name_live ON task_templates
    (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name))
    WHERE deleted_at IS NULL;
-- One live default for each type in a project, and one in the workspace.
CREATE UNIQUE INDEX IF NOT EXISTS task_templates_default_live ON task_templates
    (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), default_for)
    WHERE deleted_at IS NULL AND default_for IS NOT NULL;
