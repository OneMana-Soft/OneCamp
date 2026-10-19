-- Migration 347: the outbox (design/chat-vs-slack/PROGRAM.md §4.2 #48; chat A2
-- builds it, for the message pipeline first). The chat programme worked on it
-- as 241; it took the next free number, 347, when it merged into beta.
--
-- A row is an event a change made, written in the same transaction as the
-- change: what it is (topic, kind), what it is about (entities: ids by name,
-- never a body), which of the topic's sinks it is for (sinks; none listed is
-- all of them) and the order it keeps (key). business/Outbox hands each to its
-- topic's sinks, at least once each, and keeps what happened.
--
-- state: held until the change's other stores are written (a message's graph
--   node), then ready; done once every sink has had it; dead once the drain
--   gave up on it, kept to be seen; cancelled when the change was undone.
-- key: rows with one key reach their sinks one at a time, oldest first (a
--   conversation's messages: "channel:<uuid>"). A row in backoff doesn't hold
--   back the rows after it.
-- sinks_done: the sinks that have had it, so a retry gives it to the rest.
--
-- ids are made in Go (event_id) or by the sequence (id, the order): Postgres
-- 12 makes no uuids of its own here.
CREATE TABLE IF NOT EXISTS outbox (
    "id"            bigserial PRIMARY KEY,
    "event_id"      uuid NOT NULL UNIQUE,
    "topic"         varchar(64) NOT NULL,
    "kind"          varchar(64) NOT NULL,
    "key"           varchar(160) NOT NULL,
    "entities"      jsonb NOT NULL DEFAULT '{}',
    "actor"         uuid,
    "sinks"         text[] NOT NULL DEFAULT '{}',
    "state"         varchar(16) NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'ready', 'done', 'dead', 'cancelled')),
    "sinks_done"    text[] NOT NULL DEFAULT '{}',
    "attempts"      integer NOT NULL DEFAULT 0,
    "available_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "claimed_by"    varchar(128),
    "claimed_until" TIMESTAMP WITH TIME ZONE,
    "last_error"    text,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "finished_at"   TIMESTAMP WITH TIME ZONE
);

-- What the drain asks for: the rows still to go, oldest first.
CREATE INDEX IF NOT EXISTS outbox_pending_idx
    ON outbox (id) WHERE state IN ('held', 'ready');

-- A key's rows still to go, for the order.
CREATE INDEX IF NOT EXISTS outbox_key_idx
    ON outbox (key, id) WHERE state IN ('held', 'ready');

-- What the purge takes, by when it finished.
CREATE INDEX IF NOT EXISTS outbox_finished_idx
    ON outbox (finished_at) WHERE state IN ('done', 'dead', 'cancelled');
