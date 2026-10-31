DROP INDEX IF EXISTS calendars_one_imported_name_idx;
UPDATE event_attendees SET invite_state = 'not_sent' WHERE invite_state = 'imported';
ALTER TABLE event_attendees DROP CONSTRAINT IF EXISTS event_attendees_invite_state_check;
ALTER TABLE event_attendees ADD CONSTRAINT event_attendees_invite_state_check
    CHECK (invite_state IN ('', 'sent', 'not_sent', 'failed'));
