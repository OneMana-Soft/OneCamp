-- Calendar B1, task blocks (gcal-new-task-blocks): time on the grid for a
-- task. A task block is an event of kind 'task_block' (345_calendar_store)
-- that names its task: the task's uuid in the work graph, where tasks live,
-- so there is no foreign key. A task block names its task, and only a task
-- block names one. A task deleted later leaves its blocks as they were; a
-- read shows the task only to someone who may still read it.
--
-- Postgres 12: every statement can run twice. The number is a placeholder;
-- the integrator numbers it at landing.

ALTER TABLE events ADD COLUMN IF NOT EXISTS "task_uuid" uuid;

-- A task's blocks, for the task's side (its "Time blocked" and the work
-- graph's time_for edge): only the rows that name one.
CREATE INDEX IF NOT EXISTS events_task_idx ON events (task_uuid) WHERE task_uuid IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'events_task_only_on_blocks') THEN
        ALTER TABLE events ADD CONSTRAINT events_task_only_on_blocks
            CHECK (task_uuid IS NULL OR kind = 'task_block');
    END IF;
END $$;
