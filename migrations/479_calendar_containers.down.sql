-- The foreign key keeps an index of its own once events_by_calendar_idx goes.
CREATE INDEX IF NOT EXISTS idx_fk_events_calendar_id ON events (calendar_id);
DROP INDEX IF EXISTS events_by_calendar_idx;
DROP TABLE IF EXISTS calendar_shares;
DROP INDEX IF EXISTS calendars_one_per_container_idx;
ALTER TABLE calendars DROP COLUMN IF EXISTS container_id;
ALTER TABLE calendars DROP COLUMN IF EXISTS container_kind;
