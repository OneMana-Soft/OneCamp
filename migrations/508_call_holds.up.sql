-- What the people running a call hold for everyone else (calls meet-12 and
-- zoom-14, as Meet's host management and Zoom's Security menu).
--   call_sessions.held       what everyone who doesn't run the call may not do
--                            now: share, chat, react, microphone, camera.
--   call_attendance.allowed  what one person may do anyway, as a host let them.
-- Both end with the call: an ended call's rows are only history.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS held text[] NOT NULL DEFAULT '{}';
ALTER TABLE call_attendance ADD COLUMN IF NOT EXISTS allowed text[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_sessions_held_kind') THEN
        ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_held_kind
            CHECK (held <@ ARRAY['share', 'chat', 'react', 'microphone', 'camera']::text[]);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_attendance_allowed_kind') THEN
        ALTER TABLE call_attendance ADD CONSTRAINT call_attendance_allowed_kind
            CHECK (allowed <@ ARRAY['share', 'chat', 'react', 'microphone', 'camera']::text[]);
    END IF;
END $$;
