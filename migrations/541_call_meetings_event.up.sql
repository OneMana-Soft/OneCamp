-- An event's call knows its event (calls programme, meet-02 (2), CALLS-LEAD-73;
-- number 541 from the ledger at landing, where it was the placeholder 945).
--
-- call_meetings.event_id: the calendar event whose call the meeting is (kind
--   'event'), NULL for every other meeting. What the call makes (its
--   recording, transcript, notes and recap) lands on the event through it
--   (business/CallState HomeOf). A call taken off its event keeps it, as its
--   recordings are still the event's; an event purged for good leaves its
--   meetings without one.
-- Its index is built below, not CONCURRENTLY: a new column, partial on NOT NULL, in the ALTER's own lock (main, 30 Oct).
ALTER TABLE call_meetings ADD COLUMN IF NOT EXISTS event_id uuid REFERENCES events(id) ON DELETE SET NULL;

-- Event calls made before know their event from the calendar store, which
-- holds each event's call by its room ("meet-<call_meetings.id>"). A series
-- shares its call with its own instances, and "this and following" gives it to
-- the new series too: the call is the event's that held it first.
UPDATE call_meetings m SET event_id = h.id
FROM (
    SELECT DISTINCT ON (call_room) call_room, id
    FROM events
    WHERE call_room LIKE 'meet-%' AND series_id IS NULL
    ORDER BY call_room, created_at, id
) h
WHERE m.kind = 'event' AND m.event_id IS NULL AND h.call_room = 'meet-' || m.id;

-- An event's calls, for its recordings and notes. Not unique: a call taken off
-- and added again leaves the first one closed beside it.
CREATE INDEX IF NOT EXISTS idx_call_meetings_event ON call_meetings (event_id) WHERE event_id IS NOT NULL;
