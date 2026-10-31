-- Rules that run on time (package 9, R3; design/projects-vs-asana-jira/PROGRAM.md,
-- "R3 plan"). 492 is a placeholder: the integrator gives it the next free
-- number above beta's top when it lands.
--
-- A rule can now run on a schedule (every day, on weekdays, on chosen days,
-- every month), when a task's due date is near, or when it has passed.
--
-- Schedules ride the one queue every delayed piece of work already uses
-- (scheduled_jobs, migration 72). Each live schedule rule has one waiting job,
-- which fires and then names the next, as the calendar's reminders do
-- (migration 350). The schedule itself lives in the rule's trigger_config.
--
-- Postgres 12: every statement can run twice.

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed',
        'meeting_ended',
        'task_assigned',
        'task_field_changed',
        'schedule',
        'task_due_soon',
        'task_overdue'
    ));

-- How many tasks a scheduled run found its conditions held for. It acts on
-- at most 200 of them, and its steps say which. NULL for every other run.
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS matched integer;

-- One waiting job per schedule rule, so planning it again moves it.
CREATE UNIQUE INDEX IF NOT EXISTS scheduled_jobs_workflow_schedule_idx
    ON scheduled_jobs ((payload->>'workflow_id'))
    WHERE job_type = 'workflow_schedule' AND status = 'pending' AND deleted_at IS NULL;
