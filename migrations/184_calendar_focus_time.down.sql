DROP INDEX IF EXISTS calendar_events_focus_idx;
ALTER TABLE calendar_events DROP COLUMN IF EXISTS is_focus;
