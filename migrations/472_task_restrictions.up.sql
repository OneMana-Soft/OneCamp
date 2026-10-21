-- WA2 (design/projects-vs-asana-jira/WA2_PLAN.md): a task its project keeps
-- to a few people. A restricted task is read only by its project's admins,
-- its assignee, its creator and the people granted on it
-- (business/WorkAccess); everyone else is shown nothing of it
-- (models/taskread). Postgres, not the graph: the rule reads it with the
-- rest of a person's standing, and restricted tasks are few.
--
-- A sub-task is restricted with the task above it (via_task_id, the task the
-- restriction was set on): whoever may see that one sees it too, its people
-- are that one's, and opening that one opens it again (the cascade). One
-- restricted on its own has no via_task_id; a via_task_id always names one
-- restricted on its own.
CREATE TABLE IF NOT EXISTS task_restrictions (
    task_id       uuid PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    restricted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    restricted_at timestamptz NOT NULL DEFAULT NOW(),
    via_task_id   uuid REFERENCES task_restrictions(task_id) ON DELETE CASCADE,
    CHECK (via_task_id IS DISTINCT FROM task_id)
);
CREATE INDEX IF NOT EXISTS task_restrictions_project ON task_restrictions (project_id);
CREATE INDEX IF NOT EXISTS task_restrictions_via ON task_restrictions (via_task_id) WHERE via_task_id IS NOT NULL;
-- Every foreign key has an index leading with it (deleting a user looks here).
CREATE INDEX IF NOT EXISTS idx_fk_task_restrictions_restricted_by ON task_restrictions (restricted_by) WHERE restricted_by IS NOT NULL;

-- The people let into a restricted task beyond those the rule always lets in:
-- a task restricted on its own's, which those restricted with it share.
CREATE TABLE IF NOT EXISTS task_restriction_grants (
    task_id    uuid NOT NULL REFERENCES task_restrictions(task_id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    granted_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, user_id)
);
CREATE INDEX IF NOT EXISTS task_restriction_grants_user ON task_restriction_grants (user_id);
CREATE INDEX IF NOT EXISTS idx_fk_task_restriction_grants_granted_by ON task_restriction_grants (granted_by) WHERE granted_by IS NOT NULL;
