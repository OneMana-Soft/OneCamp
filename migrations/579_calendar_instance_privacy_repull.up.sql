-- Calendar V-613: every Google and Outlook sync calendar is read whole once.
-- Before V-613 one instance of a repeating event, private at its provider
-- on its own while its series wasn't, came in as its first override here
-- with its series' visibility (default), so who may see the calendar saw
-- its details. A read from a cursor brings only what changed since, so such
-- an override stays default until its instance changes at the provider.
-- Without a cursor the next sweep reads the calendar whole
-- (business/Calendar syncLink): every instance is taken again, its own
-- CLASS with it, and nothing is sent to the calendar until that read has
-- worked (as after 577).
--
-- Postgres 12; runs twice as it runs once. Address subscriptions keep their
-- cursors: a subscribed calendar is its person's alone (calendars.go), so
-- an instance's privacy there hides it from nobody.
UPDATE calendar_sync_calendars SET cursor = '', updated_at = NOW()
WHERE cursor <> '' AND link_id IN (SELECT id FROM calendar_sync_links WHERE provider IN ('google', 'microsoft'));
