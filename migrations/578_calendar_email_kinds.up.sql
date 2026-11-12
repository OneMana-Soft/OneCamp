-- Calendar head E (gcal-5): each person chooses the calendar's emails by kind,
-- as Google lets them: invitations (a booking of their time is one), changes
-- (a cancellation is one), and replies to events they organise. On for
-- everyone, so nobody's email changes until they turn one off. A reminder by
-- email has no switch here: the person asked for it on the event or as a
-- default. Metadata only: columns with a constant default, so nothing is
-- scanned or rewritten while the table is locked.
ALTER TABLE users_notification_preferences
    ADD COLUMN IF NOT EXISTS "email_calendar_invites" boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS "email_calendar_changes" boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS "email_calendar_answers" boolean NOT NULL DEFAULT true;
