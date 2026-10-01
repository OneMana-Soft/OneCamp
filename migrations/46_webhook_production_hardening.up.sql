-- Migration 46: Webhook production hardening
-- Add signature verification support, command registry, and delivery metadata

-- Add signature_required flag for incoming webhooks
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS signature_required boolean DEFAULT false;

-- Add command metadata JSONB for slash commands
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS commands jsonb DEFAULT '[]'::jsonb;

-- Add supports_interactive flag for outgoing webhooks
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS supports_interactive boolean DEFAULT false;

-- Index for signature_required webhooks
CREATE INDEX IF NOT EXISTS idx_webhooks_signature_required ON webhooks(signature_required) WHERE signature_required = true AND deleted_at IS NULL;

-- Table for slash command registry (webhook-scoped commands)
CREATE TABLE IF NOT EXISTS webhook_commands (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "webhook_id" uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    "command" varchar NOT NULL,
    "description" text,
    "handler_url" text,          -- Optional: POST to this URL instead of inline processing
    "response_type" varchar DEFAULT 'in_channel', -- ephemeral or in_channel
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    UNIQUE(webhook_id, command)
);

CREATE INDEX IF NOT EXISTS idx_webhook_commands_webhook_id ON webhook_commands(webhook_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_webhook_commands_command ON webhook_commands(command) WHERE deleted_at IS NULL;
