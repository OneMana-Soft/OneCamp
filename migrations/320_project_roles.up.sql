-- Roles in a project (projects programme A1, Work Access): admin, member,
-- commenter or viewer, for everyone on the project's member or admin edge in
-- the graph. The graph's edges still say who is in a project, for every read;
-- a row here says how much they may do, and where the two disagree the lower
-- holds (business/WorkAccess). Rows are filled from the graph by the
-- projects.roles.parity sweep, and with no row the graph's answer applies:
-- an admin edge an admin, a member edge a member.
--
-- user_id has no foreign key on purpose: the row mirrors a graph membership,
-- and some people (the demo's cast) are in the graph only.
CREATE TABLE IF NOT EXISTS project_roles (
    project_id uuid        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL,
    role       varchar(16) NOT NULL CHECK (role IN ('admin', 'member', 'commenter', 'viewer')),
    granted_by uuid        NULL,
    granted_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS project_roles_user ON project_roles (user_id);

-- One-time notices a person has seen (business/Notice): the admins' "Members
-- can now change tasks" is shown once to each, on whichever device.
CREATE TABLE IF NOT EXISTS user_notices_seen (
    user_id uuid        NOT NULL,
    notice  varchar(64) NOT NULL,
    seen_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, notice)
);

-- When members began to change tasks on this install: the notice is for the
-- admins of projects made before it. A fresh install has none.
INSERT INTO system_configs (key, value, updated_at)
VALUES ('projects.members_edit.since', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), NOW())
ON CONFLICT (key) DO NOTHING;

-- Project names are unique within a team, not across the workspace: a name
-- another team took no longer refuses a new project (or an import into
-- another team), and the refusal no longer says what other teams call theirs.
-- (project_name, team_id) stays unique.
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_project_name_key;
