-- Only who unsubscribed goes. deleted_at stays (345's), so a calendar kept
-- by an unsubscribe stays out of every read, as it was.
ALTER TABLE calendars DROP COLUMN IF EXISTS "deleted_by";
