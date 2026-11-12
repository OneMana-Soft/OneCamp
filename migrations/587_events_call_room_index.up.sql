-- no-transaction
-- gcal-15 head B: who may stay in an event's call room is asked when they
-- connect, and a room a series shares with a part of it split off ("this and
-- following") is found by the events holding it (models/postgres/CalendarV2
-- HoldingRoom). Most events hold no call (call_room is '', never NULL), so the
-- index keeps only those that do.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_call_room_idx ON events (call_room) WHERE call_room <> '';
