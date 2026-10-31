-- Undo 536_call_lobby_invitees: event calls' invitees knock again, like any guest.
UPDATE call_meetings SET lobby = 'guests' WHERE lobby = 'invitees';

ALTER TABLE call_meetings DROP CONSTRAINT IF EXISTS call_meetings_lobby_check;
ALTER TABLE call_meetings ADD CONSTRAINT call_meetings_lobby_check CHECK (lobby IN ('guests', 'nobody'));
