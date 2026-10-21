-- Task keys and types (projects programme A8K: Task Keys #94, Work Taxonomy
-- #95). Every project has a key ("Q4") and every task in it a number, so a
-- task reads as "Q4-12" wherever it appears. A key that is changed keeps
-- answering as an alias. Every task has a type: one of the six built in, or
-- one its project adds; none is a Task.
--
-- Keys are made in Go (business/TaskKeys), from the project's name and
-- unique across the workspace, so this migration doesn't make them. It
-- numbers the tasks already there, per project, in the order they were made.
-- The projects.keys sweep gives every project its key, and numbers any task
-- made between this migration and the new code starting.

CREATE TABLE IF NOT EXISTS project_keys (
    project_id  uuid        PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    key         varchar(5)  NOT NULL CHECK (key ~ '^[A-Z][A-Z0-9]{1,4}$'),
    -- The number the project's next task takes.
    next_number integer     NOT NULL DEFAULT 1 CHECK (next_number >= 1),
    set_by      uuid        NULL,
    set_at      timestamptz NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS project_keys_key ON project_keys (key);

-- A key a project used before: "Q4-12" still finds the task once Q4 became
-- LAUNCH. A key is either in use or an alias, never both (business/TaskKeys
-- checks it in the same transaction that changes a key).
CREATE TABLE IF NOT EXISTS project_key_aliases (
    key        varchar(5)  PRIMARY KEY CHECK (key ~ '^[A-Z][A-Z0-9]{1,4}$'),
    project_id uuid        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    retired_at timestamptz NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS project_key_aliases_project ON project_key_aliases (project_id);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS task_number integer NULL CHECK (task_number >= 1);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_project_number ON tasks (project_id, task_number) WHERE task_number IS NOT NULL;
-- The tasks the sweep still has to number: none, once it has run.
CREATE INDEX IF NOT EXISTS tasks_unnumbered ON tasks (project_id) WHERE task_number IS NULL AND project_id IS NOT NULL;

-- Number the tasks there already, oldest first, after any number a project
-- has (none, unless this runs twice).
UPDATE tasks AS t
SET task_number = n.base + n.rn
FROM (
    SELECT u.id,
           ROW_NUMBER() OVER (PARTITION BY u.project_id ORDER BY u.created_at, u.id) AS rn,
           COALESCE((SELECT MAX(m.task_number) FROM tasks m WHERE m.project_id = u.project_id), 0) AS base
    FROM tasks u
    WHERE u.project_id IS NOT NULL AND u.task_number IS NULL
) AS n
WHERE t.id = n.id;

-- The types. A built-in type has no project and a builtin name; a project's
-- own type has its project and no builtin name.
CREATE TABLE IF NOT EXISTS task_types (
    id          uuid        PRIMARY KEY,
    project_id  uuid        NULL REFERENCES projects(id) ON DELETE CASCADE,
    builtin     varchar(16) NULL UNIQUE,
    name        varchar(40) NOT NULL CHECK (btrim(name) <> ''),
    icon        varchar(24) NOT NULL,
    position    integer     NOT NULL DEFAULT 0,
    archived_at timestamptz NULL,
    created_by  uuid        NULL,
    created_at  timestamptz NOT NULL DEFAULT NOW(),
    CHECK ((project_id IS NULL) = (builtin IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS task_types_project_name ON task_types (project_id, lower(name)) WHERE project_id IS NOT NULL AND archived_at IS NULL;
CREATE INDEX IF NOT EXISTS task_types_project ON task_types (project_id) WHERE project_id IS NOT NULL;

-- The built-in types' ids never change: business/WorkTaxonomy names them.
INSERT INTO task_types (id, builtin, name, icon, position) VALUES
    ('a2573bba-d50b-44a2-b9f0-94df8d7310a7', 'task',      'Task',      'task',      0),
    ('5594da4f-672d-4b63-8887-3d4b83ae9cb5', 'bug',       'Bug',       'bug',       1),
    ('f33995b9-59c7-4e4e-8167-a6c205ca9ca9', 'feature',   'Feature',   'feature',   2),
    ('fd0c7ea2-3255-4a4e-9d50-97c30dea1b61', 'milestone', 'Milestone', 'milestone', 3),
    ('38e9401a-79ea-481d-9af1-b5411003aecd', 'approval',  'Approval',  'approval',  4),
    ('65e4944a-7ee3-436f-aceb-5f90dfc1c60d', 'request',   'Request',   'request',   5)
ON CONFLICT (id) DO NOTHING;

-- A task's type; none is a Task.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS task_type_id uuid NULL REFERENCES task_types(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS tasks_type ON tasks (project_id, task_type_id) WHERE task_type_id IS NOT NULL;
-- The foreign key's own index (tests/integration TestEveryForeignKeyHasAnIndex):
-- deleting a project's type looks for its tasks by task_type_id alone. The
-- column is new and empty here, so the build reads tasks once and writes nothing.
CREATE INDEX IF NOT EXISTS idx_fk_tasks_task_type_id ON tasks (task_type_id) WHERE task_type_id IS NOT NULL;
