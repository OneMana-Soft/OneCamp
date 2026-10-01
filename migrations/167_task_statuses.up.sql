-- A project's own task statuses ("QA", "Blocked", "Ready to ship"), made by
-- its admins.
--
-- Each belongs to one of the six built-in statuses, its category (backlog,
-- todo, inProgress, inReview, done, canceled). A task in a custom status keeps
-- task_status set to that category and carries the custom status beside it, so
-- everything that asks "is it done?" or "is it overdue?" reads the category and
-- keeps working, and only what shows or picks a status needs to know about
-- custom ones. See business/TaskStatus.
CREATE TABLE IF NOT EXISTS task_statuses (
    "id"         uuid PRIMARY KEY,
    "project_id" uuid NOT NULL,
    "name"       varchar(40) NOT NULL,
    "category"   varchar(16) NOT NULL,
    "color"      varchar(16) NOT NULL DEFAULT 'slate',
    "position"   integer NOT NULL DEFAULT 0,
    "created_by" uuid,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Names are unique within a project, whatever their case.
CREATE UNIQUE INDEX IF NOT EXISTS task_statuses_project_name_idx ON task_statuses (project_id, lower(name));
CREATE INDEX IF NOT EXISTS task_statuses_project_idx ON task_statuses (project_id, position);
