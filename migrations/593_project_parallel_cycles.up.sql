-- Migration 593 (jira-04 head C): a project's own settings, one row each, and
-- the first of them: parallel cycles. On, the project's cycles may share days
-- (two teams' sprints side by side, as Jira's parallel sprints); a task is
-- still in at most one cycle (task_cycles' key is the task). Off, the default
-- and what no row means, cycles run one after another as before. Only the
-- project's admins turn it on or off (business/Cycle/parallel.go).
CREATE TABLE IF NOT EXISTS project_settings (
    "project_uuid"    uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    "parallel_cycles" boolean NOT NULL DEFAULT false,
    "set_by"          uuid REFERENCES users(id) ON DELETE SET NULL,
    "updated_at"      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- Every foreign key has an index leading with it (deleting a user looks here).
CREATE INDEX IF NOT EXISTS idx_fk_project_settings_set_by ON project_settings (set_by) WHERE set_by IS NOT NULL;
