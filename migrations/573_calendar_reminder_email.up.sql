-- Calendar head A (gcal-5): which of a person's reminders for an event come by
-- email rather than as a pop-up. A subset of minutes, so a reminder taken off
-- stops emailing with it. Metadata only: a column with a constant default, and
-- a check NOT VALID (every row before it holds '{}', which it can't break), so
-- nothing is scanned or rewritten while the table is locked.
ALTER TABLE event_reminders ADD COLUMN IF NOT EXISTS "email_minutes" integer[] NOT NULL DEFAULT '{}';
ALTER TABLE event_reminders DROP CONSTRAINT IF EXISTS event_reminders_email_check;
ALTER TABLE event_reminders ADD CONSTRAINT event_reminders_email_check CHECK (email_minutes <@ minutes) NOT VALID;
