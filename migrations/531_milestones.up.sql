-- Migration 935 (a placeholder; the integrator numbers it): milestones
-- (linear-16). A milestone is a stage of a project's work: a name, a few words
-- on what it means, an optional target day, and its tasks. It belongs to the
-- project that made it (its home), whose admins change it, and can be shared
-- with other projects, whose tasks then join it too: one launch across the
-- projects that ship it, which Linear can't do. Each project keeps the
-- milestones it shows in its own order. A task is in at most one milestone.
CREATE TABLE IF NOT EXISTS milestones (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid" uuid NOT NULL,
    "name"         varchar(80) NOT NULL CHECK (btrim(name) <> ''),
    "description"  varchar(2000) NOT NULL DEFAULT '',
    "target_date"  date,
    "created_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_milestones_created_by ON milestones(created_by);

-- The projects a milestone shows in, its home among them, and its place in
-- each one's list.
CREATE TABLE IF NOT EXISTS milestone_projects (
    "project_uuid" uuid NOT NULL,
    "milestone_id" uuid NOT NULL REFERENCES milestones(id) ON DELETE CASCADE,
    "position"     int NOT NULL DEFAULT 0,
    PRIMARY KEY ("project_uuid", "milestone_id")
);
CREATE INDEX IF NOT EXISTS idx_milestone_projects_milestone ON milestone_projects(milestone_id);

CREATE TABLE IF NOT EXISTS task_milestones (
    "task_uuid"    uuid PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    "milestone_id" uuid NOT NULL REFERENCES milestones(id) ON DELETE CASCADE,
    "added_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_milestones_milestone ON task_milestones(milestone_id);
