-- Calendar head C, "Away, finished" (gcal-9b): a decline a person's out of
-- office made for them is recorded as such, for a whole event or series
-- (event_attendees) and for one instance (event_instance_answers), so "Out of
-- office" beside an answer is the record, never inferred from the times. Any
-- answer the person gives clears it. Declines made before this migration
-- carry no record and read as plain declines; their note (the away's
-- message) stays. Adding a column with a constant default is metadata only.
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS by_away boolean NOT NULL DEFAULT false;
ALTER TABLE event_instance_answers ADD COLUMN IF NOT EXISTS by_away boolean NOT NULL DEFAULT false;
