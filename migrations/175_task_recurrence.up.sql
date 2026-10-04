-- Migration 175: recurring tasks (Asana's "repeat"). One row per repeating
-- task, keyed by its live occurrence: completing it creates the next one and
-- the row moves to that task, so the rule follows the series.
--   rule  RRULE-lite, as business/Scheduler reads it: FREQ=DAILY|WEEKLY|
--         MONTHLY|YEARLY, optional INTERVAL=N and (weekly) BYDAY=MO,TH.
--   mode  'schedule': the next due date follows the calendar.
--         'completion': it counts from the day the task was done.
CREATE TABLE IF NOT EXISTS task_recurrences (
    "task_uuid"  uuid PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    "rule"       varchar NOT NULL,
    "mode"       varchar NOT NULL DEFAULT 'schedule' CHECK (mode IN ('schedule', 'completion')),
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
