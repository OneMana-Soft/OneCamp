-- Who said no to a DM's or a group's call ringing them (slack-06 H2's ring;
-- business/CallState Decline): one row a person a call, kept for good, so the
-- ended call's card can say "Declined by Maya" (business/Huddle endFacts). A
-- repeat changes nothing (the primary key). No foreign key: the down step of
-- call_sessions' migration must still drop it, and every read asks by a
-- call's id.
--
-- Ids are made in Go: Postgres 12 makes none of its own here.
CREATE TABLE IF NOT EXISTS call_declines (
    "session_id"  uuid NOT NULL,
    "user_uuid"   uuid NOT NULL,
    "declined_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY ("session_id", "user_uuid")
);
