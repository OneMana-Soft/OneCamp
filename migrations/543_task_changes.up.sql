-- The Work Change Log (projects WCL-1, PROGRAM.md #88): every change to a
-- task, numbered in the workspace (sequence), with its project, kind, the
-- fields' new values in the app's own answer names, who made it and from
-- where. business/WorkFeed reads it back for whoever may see it. Appends take
-- one transaction lock, so numbers commit in order. Kept 30 days; changed_at's
-- index is for the prune.
CREATE TABLE IF NOT EXISTS task_changes (
    sequence      bigserial   PRIMARY KEY,
    project_uuid  uuid        NOT NULL,
    resource_kind text        NOT NULL DEFAULT 'task',
    resource_uuid uuid        NOT NULL,
    kind          text        NOT NULL,
    diff          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    actor_uuid    uuid        NULL,
    source        text        NOT NULL DEFAULT 'app',
    restricted    boolean     NOT NULL DEFAULT false,
    changed_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_task_changes_changed_at ON task_changes (changed_at);
