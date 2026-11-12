ALTER TABLE users_notification_preferences
    DROP COLUMN IF EXISTS "email_calendar_answers",
    DROP COLUMN IF EXISTS "email_calendar_changes",
    DROP COLUMN IF EXISTS "email_calendar_invites";
