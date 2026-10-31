-- Who keeps a call recorded (meet-10, zoom-02): the person who started its
-- recording, cleared when a person stops it. A recording is LiveKit's egress in
-- the call's own room, and LiveKit closes that room while everyone is in the
-- call's breakout rooms, so the recording ends with it; the call records again,
-- as them, once its room is made again for the way back. A person's Stop is
-- never undone.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS recording_by uuid;
