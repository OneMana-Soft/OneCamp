-- Calls package C, breakout rooms (meet-10, zoom-02). A channel's call or a
-- meeting splits into numbered rooms. Each room's call is a session of its
-- own, of kind 'breakout', whose parent_session_id is the call it came from;
-- it ends with that call.

ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_kind_check;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_sessions_kind_kind') THEN
        ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_kind_kind
            CHECK (kind IN ('huddle', 'meeting', 'breakout'));
    END IF;
END $$;

ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS parent_session_id uuid REFERENCES call_sessions (id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS call_sessions_parent_idx
    ON call_sessions (parent_session_id) WHERE parent_session_id IS NOT NULL;

-- A call's rooms, while it has them: open since breakouts_open_at; whether
-- people may choose their own; when they close by themselves; and, once
-- closed, when everyone is called back (a 30-second countdown).
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS breakouts_open_at timestamptz;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS breakouts_choose boolean NOT NULL DEFAULT false;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS breakouts_ends_at timestamptz;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS breakouts_closing_at timestamptz;

CREATE TABLE IF NOT EXISTS call_breakouts (
    "parent_session_id" uuid        NOT NULL REFERENCES call_sessions (id) ON DELETE CASCADE,
    "position"          smallint    NOT NULL CHECK (position BETWEEN 1 AND 50),
    "name"              varchar(60) NOT NULL CHECK (length(btrim(name)) > 0),
    PRIMARY KEY (parent_session_id, position)
);

CREATE TABLE IF NOT EXISTS call_breakout_assignments (
    "parent_session_id" uuid         NOT NULL,
    "identity"          varchar(128) NOT NULL,
    "position"          smallint     NOT NULL,
    "assigned_by"       varchar(128) NOT NULL,
    "assigned_at"       timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (parent_session_id, identity),
    FOREIGN KEY (parent_session_id, position) REFERENCES call_breakouts (parent_session_id, position) ON DELETE CASCADE
);

-- A breakout room's call ends when its rooms close or its parent call ends.
ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_ended_reason_check;
ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_ended_reason_check
    CHECK (ended_reason IN ('last_left', 'room_finished', 'reconciled', 'host_ended', 'rooms_closed'));
