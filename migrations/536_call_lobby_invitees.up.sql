-- An event call's invitees skip its lobby (calls programme, meet-02 (1),
-- CALLS-LEAD-60; 536 from the ledger at landing, was the placeholder 942).
--
-- call_meetings.lobby gains 'invitees': the invitees of the event whose call
-- it is come in at once by their own invitation (the key their invitation's
-- link already carries); anyone else from outside waits, as with 'guests'.
-- Event calls are made with it, and those made before move to it: nobody could
-- choose an event call's lobby before, so 'guests' there was only the default.
ALTER TABLE call_meetings DROP CONSTRAINT IF EXISTS call_meetings_lobby_check;
ALTER TABLE call_meetings ADD CONSTRAINT call_meetings_lobby_check CHECK (lobby IN ('guests', 'nobody', 'invitees'));

UPDATE call_meetings SET lobby = 'invitees' WHERE kind = 'event' AND lobby = 'guests';
