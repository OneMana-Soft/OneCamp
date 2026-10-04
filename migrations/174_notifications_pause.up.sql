-- Migration 174: pause notifications (Slack's "pause notifications").
--
-- Before this, /dnd lived only in one browser's localStorage: it silenced that
-- tab while phones kept buzzing. A pause now lives on the server, and every
-- push token lookup skips a person who is paused or inside their quiet hours
-- (domain/UserFCMToken). Quiet hours used to defer email only; they now hold
-- every notification, which is what a person setting them expects.
ALTER TABLE users_notification_preferences
    ADD COLUMN IF NOT EXISTS "notifications_paused_until" TIMESTAMP WITH TIME ZONE;

-- The push gate does time-zone arithmetic in SQL, where a bad zone name or
-- time would fail the whole lookup. Writes have been validated by the API;
-- clear anything stored before that so no row can break the gate.
UPDATE users_notification_preferences
   SET quiet_hours_tz = NULL
 WHERE quiet_hours_tz IS NOT NULL
   AND quiet_hours_tz NOT IN (SELECT name FROM pg_timezone_names);
UPDATE users_notification_preferences
   SET quiet_hours_enabled = false
 WHERE quiet_hours_enabled
   AND (quiet_hours_start IS NULL OR quiet_hours_end IS NULL
        OR quiet_hours_start !~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'
        OR quiet_hours_end !~ '^([01][0-9]|2[0-3]):[0-5][0-9]$');
