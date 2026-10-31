-- Calendar B8: a calendar read from an address (SY-14): public holidays, a
-- team's calendar in another tool, someone's secret link. Each is one of the
-- person's calendars (kind 'subscribed') and one row of calendar sync's
-- ledger under their "ics" link, its remote id the address's digest. The
-- address itself is kept sealed (helpers.EncryptSecret): a calendar's
-- private link is a secret. read_at is when it was last read, or tried;
-- error, why it couldn't be, in words.
--
-- Postgres 12: every statement can run twice.

ALTER TABLE calendar_sync_calendars ADD COLUMN IF NOT EXISTS "source_sealed" text NOT NULL DEFAULT '';
ALTER TABLE calendar_sync_calendars ADD COLUMN IF NOT EXISTS "read_at" timestamptz;
ALTER TABLE calendar_sync_calendars ADD COLUMN IF NOT EXISTS "error" text NOT NULL DEFAULT '';

-- The sweep's read: subscriptions, the longest unread first.
CREATE INDEX IF NOT EXISTS calendar_sync_calendars_due_idx ON calendar_sync_calendars (read_at) WHERE source_sealed <> '';
