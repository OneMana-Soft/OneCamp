-- A4 H2 "One door for chat" (business/Notify Deliver): one row for each
-- person and each way they hear of one event, the router's record of what it
-- told whom. The unique key is the dedup: a retry or a double fire of the same
-- event inserts nothing new, so nobody is told twice. A held delivery (pause,
-- schedule, focus time) is a row in state 'held' until hold_until, for the
-- Sweeper (A4 H4). Deleting a message retracts its rows (state 'retracted', or
-- 'dropped' for what was still held); nothing here is ever deleted, and how
-- long rows are kept is the owner's call (no purge yet).
--
-- Ids only: never a body, a title or anyone's words, so no index ever holds
-- user text. message_id is the message the event is about (a post, a chat
-- message, a thread reply, the message a reaction is on), which Retract finds
-- the rows by; NULL for an event that isn't about a message.

CREATE TABLE IF NOT EXISTS notification_deliveries (
    "event_id"   varchar(200) NOT NULL,
    "user_id"    uuid NOT NULL,
    "delivery"   varchar(16) NOT NULL,
    "cause"      varchar(40) NOT NULL,
    "state"      varchar(16) NOT NULL,
    "message_id" uuid,
    "hold_until" timestamptz,
    "created_at" timestamptz NOT NULL DEFAULT NOW(),
    "sent_at"    timestamptz,
    CONSTRAINT notification_deliveries_key UNIQUE (event_id, user_id, delivery),
    CONSTRAINT notification_deliveries_state_check CHECK (state IN ('sent', 'held', 'dropped', 'retracted')),
    CONSTRAINT notification_deliveries_delivery_check CHECK (delivery IN ('inbox', 'badge', 'push', 'desktop', 'email', 'sound'))
);

CREATE INDEX IF NOT EXISTS notification_deliveries_created_at_idx
    ON notification_deliveries (created_at);

CREATE INDEX IF NOT EXISTS notification_deliveries_message_idx
    ON notification_deliveries (message_id) WHERE message_id IS NOT NULL;
