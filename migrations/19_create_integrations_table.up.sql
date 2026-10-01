CREATE TABLE IF NOT EXISTS integrations(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "entity_type" varchar NOT NULL, -- e.g., 'user', 'project', 'team', 'channel'
    "entity_id" uuid NOT NULL,
    "provider" varchar NOT NULL, -- e.g., 'google_calendar', 'slack_bot'
    "access_token" text,
    "refresh_token" text,
    "sync_token" text,
    "webhook_url" text,
    "metadata" jsonb,
    "expires_at" TIMESTAMP WITH TIME ZONE,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(entity_type, entity_id, provider)
);