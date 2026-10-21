-- Migration 244: threads you follow (design/chat-vs-slack/PROGRAM.md A8 core,
-- chat P2). The working number is the chat programme's; the integrator gives
-- it the next free number at merge.
--
-- Chat threads join the anchored threads (migrations/*_threads.up.sql) as an
-- index, not a move: a channel's, a DM's or a group's thread is a threads row
-- whose id is its root message's uuid (resource_kind channel, dm or group;
-- resource_uuid the conversation; anchor_kind 'message'), and its replies stay
-- in the graph. Who takes part comes from the replies (author, replier,
-- mentioned) and from following by hand.
--
-- follow is someone's own choice for a thread: 'auto' follows while they take
-- part (the author, a replier, someone mentioned), 'on' follows it whatever
-- their part, and 'off' never does. last_reply_at is when the thread last had
-- a reply, so a person's threads read newest first, and one is unread when it
-- had a reply after they last read it. Constant defaults and no uuids made by
-- Postgres: it all runs on Postgres 12, and adding the columns rewrites no row.
ALTER TABLE thread_participants
    ADD COLUMN IF NOT EXISTS follow text NOT NULL DEFAULT 'auto' CHECK (follow IN ('auto', 'on', 'off'));

ALTER TABLE threads ADD COLUMN IF NOT EXISTS last_reply_at timestamptz;

-- A person's threads: the Threads view's one read.
CREATE INDEX IF NOT EXISTS idx_thread_participants_user ON thread_participants (user_uuid);

-- A kind's threads by their latest reply.
CREATE INDEX IF NOT EXISTS idx_threads_last_reply ON threads (resource_kind, last_reply_at DESC);
