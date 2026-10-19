-- Migration 346: conversations, who is in them, and the message ledger (the chat
-- programme's A9: business/Conversation; design/chat-vs-slack/PROGRAM.md §4.2
-- #22). The chat programme worked on it as 240; it took the next free number,
-- 346, when it merged into beta.
--
-- conversations: one row for each channel, DM and group. ref is what the API,
--   MQTT and the web app name it by (a channel's uuid, a DM's or group's grouping
--   id); id is the row's own key, which is the channel's uuid for a channel and a
--   uuid made in Go for a DM or group. A group's ref is the hash of its people,
--   and since adding people opens the new set's conversation instead of changing
--   this one (D-S1), it never changes. A channel's name, privacy, post policy
--   and archive time stay in channels, read by join: no third copy.
--   continued_in is the private channel a group became.
--
-- conversation_members: who is in each, one row a person, kept beside the graph's
--   membership edges (written by the same functions) and compared with them
--   every night (business/Conversation reconcile.go). A removal sets left_at
--   instead of deleting the row, so a backfill running beside it can't bring the
--   person back. hidden_at is a DM or group someone hid until a newer message.
--   Notification levels and read marks are not here: they stay in
--   users_*_notification and last_seen_* (A4's and A3's).
--
-- messages: the ledger of messages, one row each, with its conversation and its
--   thread. A9 writes the copies a shared history makes (copied_from: once per
--   original and conversation); the message pipeline (A2) writes every message,
--   with the id the app gave it (client_msg_id: once per author and
--   conversation).
--
-- ids are made in Go: Postgres 12 makes none of its own here.
CREATE TABLE IF NOT EXISTS conversations (
    "id"           uuid PRIMARY KEY,
    "kind"         varchar(16) NOT NULL CHECK (kind IN ('channel', 'dm', 'group')),
    "ref"          varchar(128) NOT NULL UNIQUE,
    "continued_in" uuid REFERENCES conversations (id) ON DELETE SET NULL,
    "created_by"   uuid,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS conversation_members (
    "conversation_id" uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    "user_id"         uuid NOT NULL,
    "role"            varchar(16) NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'manager')),
    "joined_at"       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "left_at"         TIMESTAMP WITH TIME ZONE,
    "hidden_at"       TIMESTAMP WITH TIME ZONE,
    "updated_at"      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("conversation_id", "user_id")
);

-- A person's conversations, as they are now.
CREATE INDEX IF NOT EXISTS conversation_members_user_live_idx
    ON conversation_members (user_id) WHERE left_at IS NULL;

CREATE TABLE IF NOT EXISTS messages (
    "id"              uuid PRIMARY KEY,
    "conversation_id" uuid NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    "thread_root"     uuid,
    "author_id"       uuid NOT NULL,
    "client_msg_id"   varchar(64),
    "copied_from"     uuid,
    "created_at"      TIMESTAMP WITH TIME ZONE NOT NULL,
    "edited_at"       TIMESTAMP WITH TIME ZONE,
    "deleted_at"      TIMESTAMP WITH TIME ZONE
);

-- A send tried again is the same send (CH3).
CREATE UNIQUE INDEX IF NOT EXISTS messages_client_msg_idx
    ON messages (author_id, conversation_id, client_msg_id) WHERE client_msg_id IS NOT NULL;

-- A message is shared into a conversation once, however often the share runs.
CREATE UNIQUE INDEX IF NOT EXISTS messages_copied_from_idx
    ON messages (conversation_id, copied_from) WHERE copied_from IS NOT NULL;

-- A conversation's messages in time order; a thread's replies.
CREATE INDEX IF NOT EXISTS messages_conversation_time_idx
    ON messages (conversation_id, created_at);
CREATE INDEX IF NOT EXISTS messages_thread_root_idx
    ON messages (thread_root) WHERE thread_root IS NOT NULL;
