-- A call's recap on its event (gcal-new-event-notes). Each person chooses
-- whether the recap of an event's call is emailed to them, beside the
-- calendar's other emails by kind (578): on for everyone, so nobody's email
-- changes until they turn it off. And each event says whether its call's
-- recap (its words and action items, never its transcript or recording) is
-- emailed to its guests by email: its organiser's "Send the recap to guests",
-- off for every event until they turn it on. Metadata only: columns with a
-- constant default, so nothing is scanned or rewritten while a table is
-- locked (Postgres 11+).
ALTER TABLE users_notification_preferences
    ADD COLUMN IF NOT EXISTS "email_calendar_recaps" boolean NOT NULL DEFAULT true;
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS "recap_to_guests" boolean NOT NULL DEFAULT false;
