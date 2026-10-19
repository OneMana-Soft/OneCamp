-- Migration 349: answers and invitations for the calendar's events (calendar
-- programme A2, working number 261). It changes 345's tables, so it sorts
-- after them; the integrator gives it the next free number above beta's top
-- when it merges.
--
-- Guests answer yes, no or maybe, with a note, for a whole event or series or
-- for one instance of a series; people outside the workspace are invited by
-- email and answer from a link in it.
--
-- Postgres 12: ids are made in Go, and every statement can run twice.

-- A guest's answer for one instance of a repeating event alone ("this
-- event"). Their answer to the series is on event_attendees; an instance's
-- answer here comes first for that instance.
CREATE TABLE IF NOT EXISTS event_instance_answers (
    "event_id"      uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    -- The instance's original start in its series (events.recurrence_id's
    -- encoding: the date at 00:00 UTC for an all-day series).
    "recurrence_id" timestamptz NOT NULL,
    "user_id"       uuid REFERENCES users(id) ON DELETE CASCADE,
    "email"         text NOT NULL DEFAULT '',
    "response"      varchar(16) NOT NULL CHECK (response IN ('accepted', 'declined', 'tentative')),
    "note"          text NOT NULL DEFAULT '',
    "responded_at"  timestamptz NOT NULL,
    CONSTRAINT event_instance_answers_who_check CHECK (user_id IS NOT NULL OR email <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS event_instance_answers_person_idx
    ON event_instance_answers (event_id, recurrence_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS event_instance_answers_email_idx
    ON event_instance_answers (event_id, recurrence_id, lower(email)) WHERE user_id IS NULL;

-- A guest invited by email: the name they were invited by; whether their
-- invitation was emailed ('sent'), wasn't because this server sends no such
-- email ('not_sent'), or couldn't be ('failed'); their answer link, as the
-- hash it is looked up by and sealed with the server's key (helpers
-- EncryptSecret) so the organiser can copy it or have it sent again; and when
-- they were last asked again for an answer. A member's row leaves these empty.
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "name" text NOT NULL DEFAULT '';
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "invite_state" varchar(12) NOT NULL DEFAULT ''
    CHECK (invite_state IN ('', 'sent', 'not_sent', 'failed'));
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "rsvp_token_hash" bytea;
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "rsvp_token_sealed" text NOT NULL DEFAULT '';
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "nudged_at" timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS event_attendees_rsvp_token_idx
    ON event_attendees (rsvp_token_hash) WHERE rsvp_token_hash IS NOT NULL;

-- The organiser's "Email me when guests answer", per event.
ALTER TABLE events ADD COLUMN IF NOT EXISTS "email_on_answers" boolean NOT NULL DEFAULT false;
