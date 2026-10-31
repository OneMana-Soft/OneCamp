-- Meetings' homes and their lobby (calls programme, CALLS-2; 494 is a
-- placeholder, given the next free number at landing).
--
-- call_meetings: a meeting with a link of its own (an instant meeting now;
--   personal rooms and event calls follow). It is the meeting itself, never a
--   way in: who may come in from outside is a guest grant (guest_grants), under
--   the one guest-access switch.
--   lobby is who waits to be let in: 'guests' (everyone from outside, the
--   default, D-CL-5) or 'nobody' (anyone with the link joins at once).
--
-- call_knocks: someone from outside asking to join a meeting's call, and what
--   came of it. The guest waits on their page; whoever runs the call admits or
--   denies them (Call Control's admit, deny, admit_all). identity is the
--   LiveKit identity they join as once admitted.
CREATE TABLE IF NOT EXISTS call_meetings (
    "id"          varchar(64) PRIMARY KEY,
    "kind"        varchar(16) NOT NULL DEFAULT 'instant' CHECK (kind IN ('instant', 'personal', 'event')),
    "owner"       uuid NOT NULL,
    "title"       varchar(200),
    "lobby"       varchar(16) NOT NULL DEFAULT 'guests' CHECK (lobby IN ('guests', 'nobody')),
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL,
    "archived_at" TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS call_knocks (
    "id"         uuid PRIMARY KEY,
    "room"       varchar(160) NOT NULL,
    "grant_id"   uuid NOT NULL,
    "name"       varchar(80) NOT NULL,
    "identity"   varchar(128),
    "knocked_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "decided_at" TIMESTAMP WITH TIME ZONE,
    "decision"   varchar(16) CHECK (decision IN ('admitted', 'denied', 'gone')),
    "decided_by" varchar(128)
);

-- The people waiting in a room's lobby, oldest first.
CREATE INDEX IF NOT EXISTS idx_call_knocks_waiting ON call_knocks (room, knocked_at) WHERE decided_at IS NULL;
