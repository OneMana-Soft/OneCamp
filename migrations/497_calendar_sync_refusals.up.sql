-- Calendar C9 (package 25): two-way sync with Google that nothing holds up.
-- A calendar the link may only read is never written to; a change Google
-- won't take, or couldn't take for now, waits (until its retry, or until the
-- event changes) instead of being sent again every minute ahead of the rest;
-- and a link Google no longer answers for is 'broken' until the person
-- connects Google again.
--
-- Postgres 12: every statement can run twice. Only adds: a server from
-- before this keeps working on it.

ALTER TABLE calendar_sync_calendars ADD COLUMN IF NOT EXISTS "read_only" boolean NOT NULL DEFAULT false;

ALTER TABLE calendar_sync_links DROP CONSTRAINT IF EXISTS calendar_sync_links_state_check;
ALTER TABLE calendar_sync_links ADD CONSTRAINT calendar_sync_links_state_check CHECK (state IN ('moving', 'synced', 'failed', 'broken'));

CREATE TABLE IF NOT EXISTS calendar_sync_refusals (
    "link_id"    uuid NOT NULL REFERENCES calendar_sync_links(id) ON DELETE CASCADE,
    "event_id"   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "tries"      integer NOT NULL DEFAULT 1,
    -- When to send it again; NULL: not until the event changes.
    "retry_at"   timestamptz,
    "why"        text NOT NULL DEFAULT '',
    "updated_at" timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (link_id, event_id)
);

-- By event: deleting an event finds its rows here.
CREATE INDEX IF NOT EXISTS calendar_sync_refusals_event_idx ON calendar_sync_refusals (event_id);
