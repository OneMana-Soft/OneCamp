-- O-L2 (design/linear/ANALYSIS.md 7.1, option A). 486 is a placeholder: the
-- integrator gives it the next free number at landing.
--
-- The key a task had in the source it was imported from, when its project's
-- key differs: Linear's keys belong to teams, so ENG-7 can land in a project
-- keyed MOB, and ENG-7 still finds it (models/postgres/TaskKey Resolve). A
-- source key is 1 to 10 capitals and digits, starting with a capital
-- (business/TaskKeys SourceKey). A project can't take a key one of these uses.
-- The row goes with its task, and with its import when that is rolled back.
CREATE TABLE IF NOT EXISTS task_key_aliases (
    key        varchar(10) NOT NULL CHECK (key ~ '^[A-Z][A-Z0-9]{0,9}$'),
    number     integer     NOT NULL CHECK (number >= 1),
    task_id    uuid        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    import_id  uuid        NULL,
    created_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (key, number)
);

CREATE INDEX IF NOT EXISTS task_key_aliases_task ON task_key_aliases (task_id);
CREATE INDEX IF NOT EXISTS task_key_aliases_import ON task_key_aliases (import_id) WHERE import_id IS NOT NULL;
