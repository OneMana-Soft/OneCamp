DROP INDEX IF EXISTS calendar_sync_calendars_due_idx;
ALTER TABLE calendar_sync_calendars DROP COLUMN IF EXISTS "error";
ALTER TABLE calendar_sync_calendars DROP COLUMN IF EXISTS "read_at";
ALTER TABLE calendar_sync_calendars DROP COLUMN IF EXISTS "source_sealed";
