-- Calendar B6: calendar sync's ledger. A person's link to a provider (Google
-- first) and where its move and sync stand; each of the link's calendars,
-- the OneCamp calendar it comes into and the provider's cursor for it; and
-- each event's id and version at the provider, so a change on either side
-- finds the other and an admin can check what is in step.
--
-- Postgres 12: ids come from the application, and every statement can run
-- twice.

CREATE TABLE IF NOT EXISTS calendar_sync_links (
    "id"             uuid PRIMARY KEY NOT NULL,
    "person_id"      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "provider"       varchar(16) NOT NULL CHECK (provider IN ('google', 'microsoft', 'caldav', 'ics')),
    "account"        text NOT NULL DEFAULT '',
    -- 'moving' while the first move runs, then 'synced'; 'failed' with why.
    "state"          varchar(12) NOT NULL DEFAULT 'moving' CHECK (state IN ('moving', 'synced', 'failed')),
    "progress"       integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    "moved"          integer NOT NULL DEFAULT 0,
    "unchanged"      integer NOT NULL DEFAULT 0,
    "skipped"        jsonb NOT NULL DEFAULT '[]',
    "error"          text NOT NULL DEFAULT '',
    "last_synced_at" timestamptz,
    "created_at"     timestamptz NOT NULL DEFAULT NOW(),
    "updated_at"     timestamptz NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS calendar_sync_links_one_idx ON calendar_sync_links (person_id, provider);

CREATE TABLE IF NOT EXISTS calendar_sync_calendars (
    "link_id"     uuid NOT NULL REFERENCES calendar_sync_links(id) ON DELETE CASCADE,
    "remote_id"   text NOT NULL,
    "name"        text NOT NULL DEFAULT '',
    "is_primary"  boolean NOT NULL DEFAULT false,
    "calendar_id" uuid REFERENCES calendars(id) ON DELETE SET NULL,
    "cursor"      text NOT NULL DEFAULT '',
    "updated_at"  timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (link_id, remote_id)
);

-- By the calendar it comes into: deleting a calendar finds its rows here.
CREATE INDEX IF NOT EXISTS calendar_sync_calendars_calendar_idx ON calendar_sync_calendars (calendar_id);

CREATE TABLE IF NOT EXISTS calendar_sync_refs (
    "link_id"     uuid NOT NULL REFERENCES calendar_sync_links(id) ON DELETE CASCADE,
    "remote_id"   text NOT NULL,
    "external_id" text NOT NULL,
    "event_id"    uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "etag"        text NOT NULL DEFAULT '',
    "updated_at"  timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (link_id, remote_id, external_id)
);

CREATE INDEX IF NOT EXISTS calendar_sync_refs_event_idx ON calendar_sync_refs (event_id);
