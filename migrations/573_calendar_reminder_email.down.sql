ALTER TABLE event_reminders DROP CONSTRAINT IF EXISTS event_reminders_email_check;
ALTER TABLE event_reminders DROP COLUMN IF EXISTS "email_minutes";
