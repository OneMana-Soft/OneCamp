-- Back to no recap emails by kind and no recap sent to guests by email: both
-- columns go, and a build without them reads neither.
ALTER TABLE events DROP COLUMN IF EXISTS "recap_to_guests";
ALTER TABLE users_notification_preferences DROP COLUMN IF EXISTS "email_calendar_recaps";
