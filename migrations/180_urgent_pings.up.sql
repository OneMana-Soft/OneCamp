-- Migration 180: "notify anyway" (Slack's). While someone has paused
-- notifications, a person they have a DM with may send one urgent ping a day
-- that reaches their devices anyway. Each ping is recorded so it stays one.
CREATE TABLE IF NOT EXISTS urgent_pings (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "sender_id"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "recipient_id" uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_urgent_pings_pair ON urgent_pings(sender_id, recipient_id, created_at);
