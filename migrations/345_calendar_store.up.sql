-- Migration 345: the calendar's one store (calendar programme A1, D-CAL-1).
-- The calendar programme worked on it as 260; it took the next free number,
-- 345, when it merged into beta.
--
-- Every event was two writes with no transaction: a calendar_events row and a
-- Dgraph Event node, read from Dgraph except for focus time, with away and the
-- guest list only in Dgraph. These tables become the calendar's one store when
-- CALENDAR_STORE is 'postgres'; until then nothing reads them, and
-- calendar_events and the Dgraph nodes stay exactly as they were, so the down
-- migration is a clean return to before.
--
-- Postgres 12: ids come from the application (no gen_random_uuid), and every
-- statement can run twice.

-- A calendar. Each person has one personal calendar, made when they first need
-- it; team, project and channel calendars, sharing and subscriptions come with
-- their own packages (A6, B8), which is why the kinds are named here.
CREATE TABLE IF NOT EXISTS calendars (
    "id"         uuid PRIMARY KEY NOT NULL,
    "kind"       varchar(16) NOT NULL DEFAULT 'personal'
                 CHECK (kind IN ('personal', 'team', 'project', 'channel', 'resource', 'subscribed', 'holidays', 'external')),
    "owner_id"   uuid REFERENCES users(id) ON DELETE CASCADE,
    "name"       varchar(120) NOT NULL DEFAULT '',
    "hue"        varchar(16) NOT NULL DEFAULT '',
    -- IANA zone: a new event's zone when its writer names none.
    "tz"         varchar(64) NOT NULL DEFAULT 'UTC',
    "created_at" timestamptz NOT NULL DEFAULT NOW(),
    "updated_at" timestamptz NOT NULL DEFAULT NOW(),
    "deleted_at" timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS calendars_one_personal_idx
    ON calendars (owner_id) WHERE kind = 'personal' AND deleted_at IS NULL;

-- An event: a single event, a repeating series, or one changed instance of a
-- series (an override), in one table so one read finds all three.
CREATE TABLE IF NOT EXISTS events (
    "id"           uuid PRIMARY KEY NOT NULL,
    "calendar_id"  uuid NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    "kind"         varchar(24) NOT NULL DEFAULT 'event'
                   CHECK (kind IN ('event', 'focus', 'out_of_office', 'working_location', 'task_block', 'booking', 'reminder')),
    "title"        text NOT NULL DEFAULT '',
    "notes"        text NOT NULL DEFAULT '',
    "notes_format" varchar(8) NOT NULL DEFAULT 'text' CHECK (notes_format IN ('text', 'html')),
    "location"     text NOT NULL DEFAULT '',
    -- The event's own call (B11 fills it).
    "call_room"    text NOT NULL DEFAULT '',

    -- When. A timed event has instants and the zone it was planned in (and,
    -- for a single event, the end's own zone: a flight). An all-day event has
    -- dates, the end the day after its last, the same day everywhere.
    "start_at"     timestamptz,
    "end_at"       timestamptz,
    "start_date"   date,
    "end_date"     date,
    "tz"           varchar(64) NOT NULL DEFAULT 'UTC',
    "end_tz"       varchar(64) NOT NULL DEFAULT '',

    -- Repeating (A7). A series has an RFC 5545 rule, expanded in its zone, and
    -- until_at, the last start it can have (NULL: no end), which bounds a read
    -- of a range. An override names its series and the instance it replaces by
    -- that instance's original start (RECURRENCE-ID); for an all-day series
    -- that, exdates and rdates are the date at 00:00 UTC. changed lists the
    -- fields an override set by hand, which a change to the whole series keeps.
    "rrule"         text NOT NULL DEFAULT '',
    "until_at"      timestamptz,
    "series_id"     uuid REFERENCES events(id) ON DELETE CASCADE,
    "recurrence_id" timestamptz,
    "exdates"       timestamptz[] NOT NULL DEFAULT '{}',
    "rdates"        timestamptz[] NOT NULL DEFAULT '{}',
    "changed"       text[] NOT NULL DEFAULT '{}',

    "transparency" varchar(12) NOT NULL DEFAULT 'opaque' CHECK (transparency IN ('opaque', 'transparent')),
    "visibility"   varchar(12) NOT NULL DEFAULT 'default' CHECK (visibility IN ('default', 'public', 'private')),
    "hue"          varchar(16) NOT NULL DEFAULT '',
    "status"       varchar(12) NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'tentative', 'cancelled')),
    "guests_can_modify"     boolean NOT NULL DEFAULT false,
    "guests_can_invite"     boolean NOT NULL DEFAULT true,
    "guests_can_see_guests" boolean NOT NULL DEFAULT true,

    -- The organiser.
    "created_by"   uuid REFERENCES users(id) ON DELETE CASCADE,
    -- Every write adds one; a write that names an older sequence is refused.
    "sequence"     integer NOT NULL DEFAULT 0,
    "ical_uid"     text NOT NULL,
    -- 'onecamp', or 'moved' for an event the move brought from the old stores.
    "source"       varchar(16) NOT NULL DEFAULT 'onecamp',
    "google_event_id" text,

    "created_at"   timestamptz NOT NULL DEFAULT NOW(),
    "updated_at"   timestamptz NOT NULL DEFAULT NOW(),
    -- In the trash since then.
    "deleted_at"   timestamptz,
    "deleted_by"   uuid,

    CONSTRAINT events_when_check CHECK (
        (start_at IS NOT NULL AND end_at IS NOT NULL AND start_date IS NULL AND end_date IS NULL AND end_at >= start_at)
        OR (start_date IS NOT NULL AND end_date IS NOT NULL AND start_at IS NULL AND end_at IS NULL AND end_date > start_date)
    ),
    CONSTRAINT events_override_check CHECK ((series_id IS NULL) = (recurrence_id IS NULL)),
    CONSTRAINT events_series_check CHECK (rrule = '' OR series_id IS NULL)
);

-- A person's events by time: what a range read walks.
CREATE INDEX IF NOT EXISTS events_by_creator_start_idx
    ON events (created_by, start_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS events_by_creator_date_idx
    ON events (created_by, start_date) WHERE deleted_at IS NULL;
-- Series, by how far they reach.
CREATE INDEX IF NOT EXISTS events_series_idx
    ON events (created_by, until_at) WHERE rrule <> '' AND deleted_at IS NULL;
-- One override per instance of a series.
CREATE UNIQUE INDEX IF NOT EXISTS events_override_idx
    ON events (series_id, recurrence_id) WHERE series_id IS NOT NULL;
-- One iCalendar UID per event or series; overrides share their series'.
CREATE UNIQUE INDEX IF NOT EXISTS events_ical_uid_idx
    ON events (ical_uid) WHERE series_id IS NULL;
-- The push gate: someone's focus time running now.
CREATE INDEX IF NOT EXISTS events_focus_idx
    ON events (created_by, start_at, end_at) WHERE kind = 'focus' AND deleted_at IS NULL;
-- The trash.
CREATE INDEX IF NOT EXISTS events_trash_idx
    ON events (created_by, deleted_at) WHERE deleted_at IS NOT NULL;

-- Who is invited: a workspace member by id, or (A2) someone by email. The
-- organiser is events.created_by, never an attendee of their own event.
CREATE TABLE IF NOT EXISTS event_attendees (
    "event_id"     uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "user_id"      uuid REFERENCES users(id) ON DELETE CASCADE,
    "email"        text NOT NULL DEFAULT '',
    "role"         varchar(12) NOT NULL DEFAULT 'required' CHECK (role IN ('required', 'optional')),
    "response"     varchar(16) NOT NULL DEFAULT 'needs_action'
                   CHECK (response IN ('needs_action', 'accepted', 'declined', 'tentative')),
    "note"         text NOT NULL DEFAULT '',
    "added_at"     timestamptz NOT NULL DEFAULT NOW(),
    "responded_at" timestamptz,
    CONSTRAINT event_attendees_who_check CHECK (user_id IS NOT NULL OR email <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS event_attendees_person_idx
    ON event_attendees (event_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS event_attendees_email_idx
    ON event_attendees (event_id, lower(email)) WHERE user_id IS NULL;
-- The events someone is invited to.
CREATE INDEX IF NOT EXISTS event_attendees_by_person_idx
    ON event_attendees (user_id, event_id) WHERE user_id IS NOT NULL;
