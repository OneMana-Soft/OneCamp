-- Back to before the calendar's one store: calendar_events and the Dgraph
-- Event nodes were never touched, so dropping the new tables is all it takes.
DROP TABLE IF EXISTS event_attendees;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS calendars;
