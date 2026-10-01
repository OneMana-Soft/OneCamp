CREATE TABLE IF NOT EXISTS webhooks (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "name" varchar NOT NULL,
    "description" text,
    "type" varchar NOT NULL CHECK (type IN ('incoming', 'outgoing')),
    "token" varchar NOT NULL UNIQUE,
    "secret" varchar,
    "target_url" text,
    "channel_id" uuid REFERENCES channels(id),
    "events" jsonb,
    "is_active" boolean NOT NULL DEFAULT true,
    "created_by" uuid NOT NULL REFERENCES users(id),
    "bot_name" varchar DEFAULT 'Webhook Bot',
    "bot_avatar_url" text,
    "metadata" jsonb,
    "last_triggered_at" TIMESTAMP WITH TIME ZONE,
    "failure_count" int NOT NULL DEFAULT 0,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_webhooks_token ON webhooks(token) WHERE deleted_at IS NULL;
CREATE INDEX idx_webhooks_type ON webhooks(type) WHERE deleted_at IS NULL;
CREATE INDEX idx_webhooks_channel_id ON webhooks(channel_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_webhooks_is_active ON webhooks(is_active) WHERE deleted_at IS NULL;
