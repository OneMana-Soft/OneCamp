-- Migration 242: pinned messages (design/chat-vs-slack/PROGRAM.md B25, D-M13).
-- The working number is the chat programme's; the integrator gives it the
-- next free number at merge.
--
-- A message pinned to its conversation, so everyone in it can find it again:
-- a channel's message, or a DM's or group's. The conversation is named as the
-- web app and business/Conversation name it (kind, and its id: a channel's
-- uuid, a DM's or group's grouping id, which isn't a uuid), and the message
-- by its uuid. The message itself stays in the graph; a pin of one that was
-- deleted is dropped when the pins are next read.
--
-- At most 100 pins a conversation (business/Pin). Nothing here needs a uuid
-- made by Postgres, so it all runs on Postgres 12.
CREATE TABLE IF NOT EXISTS message_pins (
    conversation_kind text        NOT NULL CHECK (conversation_kind IN ('channel', 'dm', 'group')),
    conversation_id   text        NOT NULL CHECK (length(conversation_id) BETWEEN 1 AND 128),
    message_id        uuid        NOT NULL,
    pinned_by         uuid        NOT NULL,
    pinned_at         timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (conversation_kind, conversation_id, message_id)
);

-- A conversation's pins, newest first: the one read the pins list makes.
CREATE INDEX IF NOT EXISTS idx_message_pins_recent ON message_pins (conversation_kind, conversation_id, pinned_at DESC);
