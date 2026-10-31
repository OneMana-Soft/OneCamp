-- Calendar A4: a guest a calendar file brought in keeps the answer they gave
-- elsewhere, and is never invited or told by the import ('imported').
ALTER TABLE event_attendees DROP CONSTRAINT IF EXISTS event_attendees_invite_state_check;
ALTER TABLE event_attendees ADD CONSTRAINT event_attendees_invite_state_check
    CHECK (invite_state IN ('', 'sent', 'not_sent', 'failed', 'imported'));

-- A calendar brought from another app (a Google export's other calendars)
-- is its owner's, by its name: importing the file again finds it.
CREATE UNIQUE INDEX IF NOT EXISTS calendars_one_imported_name_idx
    ON calendars (owner_id, name) WHERE kind = 'external' AND deleted_at IS NULL;
