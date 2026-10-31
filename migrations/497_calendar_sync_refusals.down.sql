DROP TABLE IF EXISTS calendar_sync_refusals;

-- A broken link reads as a failed one before this: connect Google again.
UPDATE calendar_sync_links SET state = 'failed' WHERE state = 'broken';
ALTER TABLE calendar_sync_links DROP CONSTRAINT IF EXISTS calendar_sync_links_state_check;
ALTER TABLE calendar_sync_links ADD CONSTRAINT calendar_sync_links_state_check CHECK (state IN ('moving', 'synced', 'failed'));

ALTER TABLE calendar_sync_calendars DROP COLUMN IF EXISTS read_only;
