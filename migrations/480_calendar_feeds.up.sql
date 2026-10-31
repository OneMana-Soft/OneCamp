-- Calendar B8: a secret feed of a calendar, one person's (SY-11, basecamp-05).
-- Any calendar its person may see: their own, one an import made, a
-- project's. The link is a bearer secret: only its SHA-256 is kept (as API
-- tokens are), so it is shown once when made; reset revokes it and makes
-- another, and a revoked or unknown link answers nothing at once. Busy shows
-- only when, full what too.
--
-- Postgres 12: ids come from the application; every statement can run twice.

CREATE TABLE IF NOT EXISTS calendar_feeds (
    "id"          uuid PRIMARY KEY NOT NULL,
    "calendar_id" uuid NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    "person_id"   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "token_hash"  varchar(64) NOT NULL,
    "scope"       varchar(8) NOT NULL DEFAULT 'full' CHECK (scope IN ('busy', 'full')),
    "created_at"  timestamptz NOT NULL DEFAULT NOW(),
    "revoked_at"  timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS calendar_feeds_token_idx ON calendar_feeds (token_hash);
-- One live feed per person and calendar.
CREATE UNIQUE INDEX IF NOT EXISTS calendar_feeds_live_idx ON calendar_feeds (calendar_id, person_id) WHERE revoked_at IS NULL;
-- Deleting a calendar or a person finds their feeds, revoked ones too.
CREATE INDEX IF NOT EXISTS calendar_feeds_calendar_idx ON calendar_feeds (calendar_id);
CREATE INDEX IF NOT EXISTS calendar_feeds_person_idx ON calendar_feeds (person_id);
