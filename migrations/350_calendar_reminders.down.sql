-- Migration 362 down: the calendar's reminders go. Their waiting jobs are
-- cancelled first, so nothing fires for a reminder that no longer exists.
UPDATE scheduled_jobs SET status = 'cancelled', updated_at = NOW()
    WHERE job_type = 'calendar_reminder' AND status IN ('pending', 'running');
DROP INDEX IF EXISTS scheduled_jobs_calendar_reminder_event_idx;
DROP INDEX IF EXISTS scheduled_jobs_calendar_reminder_idx;
DROP TABLE IF EXISTS event_reminders;
