-- Migration 81: admin-authorized model allowlist + per-user model choice.
--
-- Until now ai_settings pinned ONE active chat model for the whole
-- workspace. This migration lets an admin authorize a SET of models
-- (drawn from the already-configured ai_providers) that members may then
-- pick from for their personal AI assistant. The workspace default
-- (ai_settings.chat_provider_id + chat_model) is always usable and is the
-- fallback whenever a user has no pick, or their pick was revoked.
--
--   * ai_authorized_models     — the allowlist an admin manages.
--   * user_ai_model_preference — a member's chosen model (FK into the
--     allowlist; ON DELETE SET NULL so revoking a model silently reverts
--     affected users to the workspace default).
--
-- Models authorized here are still gated by the provider's own enabled
-- flag and credentials, so revoking a provider disables its models too.
-- Idempotent: safe to re-run.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS ai_authorized_models (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "provider_id" uuid NOT NULL REFERENCES ai_providers(id) ON DELETE CASCADE,
    -- Model tag/name as the provider expects it (e.g. 'llama3.2:3b',
    -- 'gpt-4o-mini', 'claude-3-5-sonnet-latest').
    "model"       varchar NOT NULL,
    -- Optional friendly name shown in the picker; empty falls back to model.
    "label"       varchar NOT NULL DEFAULT '',
    "enabled"     boolean NOT NULL DEFAULT true,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (provider_id, model)
);

CREATE INDEX IF NOT EXISTS idx_ai_authorized_models_provider
    ON ai_authorized_models (provider_id);

CREATE TABLE IF NOT EXISTS user_ai_model_preference (
    -- Dgraph user uuid (the same identity used by sessions, memory, nudges).
    "user_uuid"           varchar PRIMARY KEY,
    "authorized_model_id" uuid REFERENCES ai_authorized_models(id) ON DELETE SET NULL,
    "updated_at"          TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Seed the allowlist with the current workspace default so the admin sees
-- at least one authorized model and members have something to pick.
INSERT INTO ai_authorized_models ("provider_id", "model", "label")
SELECT chat_provider_id, chat_model, chat_model
FROM ai_settings
WHERE id = 1 AND chat_provider_id IS NOT NULL AND chat_model <> ''
ON CONFLICT ("provider_id", "model") DO NOTHING;
