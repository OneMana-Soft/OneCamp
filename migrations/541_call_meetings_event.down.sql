-- Undo 541_call_meetings_event: meetings forget their events.
DROP INDEX IF EXISTS idx_call_meetings_event;
ALTER TABLE call_meetings DROP COLUMN IF EXISTS event_id;
