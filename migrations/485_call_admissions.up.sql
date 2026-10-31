-- Members let into a call through a link, as themselves (calls HU13; 496 is a
-- placeholder, given the next free number at landing).
--
-- call_admissions: a member who opened a link while signed in. At a meeting's
--   link or a room's address (session_id NULL) they may be in that meeting's
--   call for as long as the link lives. Through a call's invite (a guest grant
--   of resource 'call') they may be in that one call (session_id), never a
--   later one in the same conversation, and never its conversation. Every
--   check also asks that the grant be live (business/Guest MemberAdmitted).
--   identity is their call identity (graph uid), to put them out when the
--   link ends. No foreign keys: the down steps of the migrations that made
--   guest_grants and call_sessions must still drop them; every read joins a
--   live grant and a live call, and a call's end forgets its admissions.
--
-- call_attendance.role gains 'invitee': a member in a conversation's call
--   through an invite, who isn't in the conversation. They run nothing, even
--   in a DM or a group, and the call is never handed to them.
--
-- Ids are made in Go: Postgres 12 makes none of its own here.
CREATE TABLE IF NOT EXISTS call_admissions (
    "room"        varchar(160) NOT NULL,
    "user_uuid"   uuid NOT NULL,
    "identity"    varchar(128) NOT NULL,
    "grant_id"    uuid NOT NULL,
    "session_id"  uuid,
    "admitted_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY ("room", "user_uuid", "grant_id")
);

-- Who came in through a link: when it ends, they are put out.
CREATE INDEX IF NOT EXISTS call_admissions_grant_idx ON call_admissions (grant_id);

-- The calls a member may be in through links, asked at every token and connect.
CREATE INDEX IF NOT EXISTS call_admissions_user_idx ON call_admissions (user_uuid);

ALTER TABLE call_attendance DROP CONSTRAINT IF EXISTS call_attendance_role_check;
ALTER TABLE call_attendance ADD CONSTRAINT call_attendance_role_check
    CHECK (role IN ('host', 'cohost', 'participant', 'guest', 'invitee'));
