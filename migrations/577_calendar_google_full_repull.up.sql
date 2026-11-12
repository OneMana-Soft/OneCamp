-- Calendar B7 (CR-B17 M1): every Google sync calendar is read whole once.
-- Before B7 neither side told the other whether an event is private: one
-- private at Google came in default, and a read from a cursor brings only
-- what changed since, so it stays default here until it changes at Google,
-- and an edit here would send that stale default to Google. Without a cursor
-- the next sweep reads the calendar whole (business/Calendar syncLink): an
-- event private on either side is made private on both, and nothing is sent
-- to the calendar until that read has worked.
--
-- Postgres 12; runs twice as it runs once. Outlook links (new with B7) and
-- address subscriptions keep their cursors.
UPDATE calendar_sync_calendars SET cursor = '', updated_at = NOW()
WHERE cursor <> '' AND link_id IN (SELECT id FROM calendar_sync_links WHERE provider = 'google');
