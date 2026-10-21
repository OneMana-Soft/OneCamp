-- Migration 361: webhook deliveries (platform P-API part 2,
-- design/platform/API_CONTRACT.md §3.3). Provisional number in the platform's
-- range (360-379); the integrator renumbers it to the next free one.
--
-- An outgoing webhook was sent its events by a goroutine that tried four times
-- in about seven seconds and forgot the rest: a restart, a deploy or an
-- endpoint down for a minute lost them, and each try carried a new delivery id,
-- so a receiver couldn't tell a retry from a new event. Now each (webhook,
-- event) is a row, written before anything is sent:
--
-- payload: exactly what the webhook is sent, as the dispatcher cut it for this
--   webhook (the event whole, or its ids: business/Webhook outgoingPrivacy.go).
--   A retry and a replay send the same body with the same event id.
-- event_id: the event's id, the same on every try and every replay (the
--   X-OneCamp-Delivery header and the body's event_id): unique per webhook,
--   so a source that runs twice queues one delivery.
-- state: pending until a try succeeds (done), or until the last try fails
--   (dead, kept 30 days to be seen and replayed), or until the webhook is gone
--   (cancelled). A paused webhook's deliveries wait, pending.
--
-- ids come from the sequence (id) or from Go (event_id): Postgres 12 makes no
-- uuids of its own.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    "id"            bigserial PRIMARY KEY,
    "webhook_id"    uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    "event_id"      uuid NOT NULL,
    "event_type"    varchar(128) NOT NULL,
    "payload"       jsonb NOT NULL,
    "state"         varchar(16) NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'dead', 'cancelled')),
    "attempts"      integer NOT NULL DEFAULT 0,
    "next_at"       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "claimed_by"    varchar(128),
    "claimed_until" TIMESTAMP WITH TIME ZONE,
    "last_status"   integer,
    "last_error"    text,
    "last_ms"       integer,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "finished_at"   TIMESTAMP WITH TIME ZONE,
    UNIQUE ("webhook_id", "event_id")
);

-- What the drain asks for: the pending rows that are due.
CREATE INDEX IF NOT EXISTS webhook_deliveries_due ON webhook_deliveries ("next_at") WHERE "state" = 'pending';
-- What the admin's list asks for: a webhook's latest deliveries.
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_webhook ON webhook_deliveries ("webhook_id", "id" DESC);
-- What the purge asks for.
CREATE INDEX IF NOT EXISTS webhook_deliveries_finished ON webhook_deliveries ("finished_at") WHERE "state" <> 'pending';

-- A webhook can be paused: its deliveries wait, and go when it's resumed. It
-- is paused by an admin, or by itself after 50 failed tries in a row (its
-- creator is told); failure_count counts the tries in a row, as it did.
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS "paused_at" TIMESTAMP WITH TIME ZONE;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS "paused_reason" varchar(32);
