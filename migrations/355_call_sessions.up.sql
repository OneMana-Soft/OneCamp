-- Migration 280 (working number in the calls programme's range; the integrator
-- gives it the next free number at merge): Call State, the record of every call
-- (design/calls-vs-zoom/PROGRAM.md §4 #29, built once for calls and chat B7 by
-- the contract agreed on 17 Oct 2026).
--
-- Until now a call was a LiveKit room and nothing else: no start, no end, no
-- list of who was in it, and every recording, transcript and recap of every call
-- a conversation ever had hung off the same room name.
--
-- call_sessions: one row a call, from the first person who joins to the moment
--   the last one leaves. room is the LiveKit room (a channel's uuid, a DM's or
--   group's grouping id, an instant meeting's "meet-<id>"), and only one session
--   per room is open at a time. resource_kind and resource_id name what the call
--   belongs to as business/Conversation's Ref does (channel, dm, group), or the
--   meeting. post_uuid and topic are the huddle post and its name, written by
--   chat's huddles (P10) through business/CallState. locked_at, spotlight and
--   mute_on_entry are what the people running the call set (Call Control).
--
-- call_attendance: who was in each call, one row a person, keyed by their
--   LiveKit identity so guests are counted too (user_uuid is empty for them).
--   joined_at is their first join, last_joined_at the latest, left_at when they
--   last left (empty while they are in), seconds_in their time across rejoins.
--   role is their part in this call: host, cohost, participant or guest.
--   What hosts do to a call (mute, remove, lock, end for everyone) is recorded
--   in the audit log, under the calls category.
--
-- call_recordings: the recordings made during a call, so a call's record links
--   them. The recording package (CALLS-6) adds the rest of a recording's fields.
--
-- Ids are made in Go: Postgres 12 makes none of its own here.
CREATE TABLE IF NOT EXISTS call_sessions (
    "id"            uuid PRIMARY KEY,
    "room"          varchar(160) NOT NULL,
    "room_sid"      varchar(64),
    "kind"          varchar(16) NOT NULL CHECK (kind IN ('huddle', 'meeting')),
    "resource_kind" varchar(16) NOT NULL CHECK (resource_kind IN ('channel', 'dm', 'group', 'meeting')),
    "resource_id"   varchar(128) NOT NULL,
    "post_uuid"     uuid,
    "topic"         varchar(200),
    "started_by"    uuid,
    "started_at"    TIMESTAMP WITH TIME ZONE NOT NULL,
    "ended_at"      TIMESTAMP WITH TIME ZONE,
    "ended_reason"  varchar(16) CHECK (ended_reason IN ('last_left', 'room_finished', 'reconciled', 'host_ended')),
    "locked_at"     TIMESTAMP WITH TIME ZONE,
    "locked_by"     varchar(128),
    "spotlight"     varchar(128),
    "mute_on_entry" boolean NOT NULL DEFAULT false
);

-- One open call per room: the join that opens it wins, every other join finds it.
CREATE UNIQUE INDEX IF NOT EXISTS call_sessions_open_room_idx
    ON call_sessions (room) WHERE ended_at IS NULL;

-- A conversation's calls, newest first (its current call, its history).
CREATE INDEX IF NOT EXISTS call_sessions_resource_idx
    ON call_sessions (resource_kind, resource_id, started_at DESC);

-- The calls whose recap, notes and transcript are filed by LiveKit's id for the
-- call (helpers.CallSessionKeyFor).
CREATE INDEX IF NOT EXISTS call_sessions_room_sid_idx
    ON call_sessions (room_sid) WHERE room_sid IS NOT NULL;

CREATE TABLE IF NOT EXISTS call_attendance (
    "session_id"     uuid NOT NULL REFERENCES call_sessions (id) ON DELETE CASCADE,
    "identity"       varchar(128) NOT NULL,
    "user_uuid"      uuid,
    "name"           varchar(200) NOT NULL DEFAULT '',
    "role"           varchar(16) NOT NULL DEFAULT 'participant' CHECK (role IN ('host', 'cohost', 'participant', 'guest')),
    "joined_at"      TIMESTAMP WITH TIME ZONE NOT NULL,
    "last_joined_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "left_at"        TIMESTAMP WITH TIME ZONE,
    "seconds_in"     integer NOT NULL DEFAULT 0,
    "removed_by"     varchar(128),
    PRIMARY KEY ("session_id", "identity")
);

-- Whether someone is in a call now ("In a call" wherever their presence shows).
CREATE INDEX IF NOT EXISTS call_attendance_user_live_idx
    ON call_attendance (user_uuid) WHERE left_at IS NULL AND user_uuid IS NOT NULL;

CREATE TABLE IF NOT EXISTS call_recordings (
    "egress_id"  varchar(64) PRIMARY KEY,
    "session_id" uuid NOT NULL REFERENCES call_sessions (id) ON DELETE CASCADE,
    "started_by" uuid,
    "started_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "ended_at"   TIMESTAMP WITH TIME ZONE,
    "status"     varchar(16) NOT NULL DEFAULT 'recording'
);

CREATE INDEX IF NOT EXISTS call_recordings_session_idx
    ON call_recordings (session_id);
