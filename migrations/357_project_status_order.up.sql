-- A project's board column order (design/projects-vs-asana-jira/
-- BOARD_COLUMNS_PLAN.md): its statuses in the order its admins dragged them,
-- each a built-in key ("todo") or a custom status's id. Everything that
-- lists a project's statuses in order follows it: board columns, the list's
-- groups, pickers. A project with no row keeps the built-in order, each
-- built-in followed by its own statuses (business/TaskStatus).
CREATE TABLE IF NOT EXISTS project_status_order (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    statuses   text[] NOT NULL,
    set_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    set_at     timestamptz NOT NULL DEFAULT NOW()
);
