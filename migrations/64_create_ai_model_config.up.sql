-- Migration 64: Admin-managed, model-agnostic AI configuration.
--
-- Before this migration, AI provider/model selection lived entirely in
-- environment variables (AI_PROVIDER, OLLAMA_MODEL, OPENAI_API_KEY, ...)
-- and was read once at boot in services/AI/config.go.
--
-- This migration moves that configuration into the database so an admin
-- can manage providers and models from the admin panel at runtime:
--   * ai_providers — connection config for each provider (Ollama, OpenAI,
--     Anthropic, and arbitrary OpenAI-compatible custom endpoints such as
--     vLLM / LM Studio / OpenRouter / a self-hosted llama.cpp server).
--   * ai_settings  — the single active selection (which provider+model is
--     used for chat, which for embeddings) plus global toggles.
--
-- API keys are stored AES-256-GCM encrypted at the application layer
-- (see models/postgres/AI/crypto.go), mirroring import_oauth_tokens.
-- pgcrypto is enabled so a future server-side encryption switch is a
-- one-query change if ever needed.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- =====================================================================
-- 1. ai_providers — one row per configured provider connection
-- =====================================================================
CREATE TABLE IF NOT EXISTS ai_providers (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- kind drives which client implementation is used:
    --   ollama            → local Ollama server (install/delete models)
    --   openai            → api.openai.com
    --   anthropic         → api.anthropic.com
    --   openai_compatible → any OpenAI /v1-compatible endpoint (custom)
    "kind"         varchar NOT NULL
        CHECK (kind IN ('ollama','openai','anthropic','openai_compatible')),
    "label"        varchar NOT NULL,
    -- Base URL. Empty string means "use the built-in default for this
    -- kind" (resolved in the loader with env fallback). Required and
    -- non-empty for openai_compatible.
    "base_url"     text NOT NULL DEFAULT '',
    -- AES-256-GCM ciphertext of the API key; NULL for keyless providers
    -- (a local Ollama server typically needs no key).
    "api_key_enc"  bytea,
    "enabled"      boolean NOT NULL DEFAULT true,
    -- Built-in rows are seeded below and cannot be deleted by the admin
    -- (only edited). Custom endpoints are is_builtin = false.
    "is_builtin"   boolean NOT NULL DEFAULT false,
    -- Opt-in: skip TLS certificate verification for this provider. Only
    -- meaningful for HTTPS custom endpoints using self-signed certs on a
    -- trusted internal network. Defaults false (verify, the safe default).
    "insecure_tls" boolean NOT NULL DEFAULT false,
    "metadata"     jsonb NOT NULL DEFAULT '{}',
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Case-insensitive unique label so the FE can show stable names.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_providers_label
    ON ai_providers (lower(label));

-- Seed the three built-in providers with fixed UUIDs so ai_settings can
-- reference them deterministically and the loader can find them without
-- a name lookup. base_url is left empty; the loader fills the kind's
-- default (and env fallback) at runtime.
INSERT INTO ai_providers ("id", "kind", "label", "base_url", "is_builtin") VALUES
    ('00000000-0000-0000-0000-0000000000a1', 'ollama',    'Local Ollama', '', true),
    ('00000000-0000-0000-0000-0000000000a2', 'openai',    'OpenAI',       '', true),
    ('00000000-0000-0000-0000-0000000000a3', 'anthropic', 'Anthropic',    '', true)
ON CONFLICT ("id") DO NOTHING;

-- =====================================================================
-- 2. ai_settings — singleton row holding the active selection
-- =====================================================================
CREATE TABLE IF NOT EXISTS ai_settings (
    -- Enforce a single row: id is pinned to 1.
    "id"                    smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    "enabled"               boolean NOT NULL DEFAULT true,

    -- Active chat / completion model.
    "chat_provider_id"      uuid REFERENCES ai_providers(id) ON DELETE SET NULL,
    "chat_model"            varchar NOT NULL DEFAULT '',

    -- Active embedding model. Kept separate because changing it has a
    -- much heavier blast radius (the OpenSearch k-NN index dimension is
    -- pinned, so a dimension change requires a re-index).
    "embedding_provider_id" uuid REFERENCES ai_providers(id) ON DELETE SET NULL,
    "embedding_model"       varchar NOT NULL DEFAULT '',
    -- Dimension of the vectors the active embedding model produces. Must
    -- match the live ai_embeddings index; the reindex flow updates both
    -- atomically.
    "embedding_dimension"   int NOT NULL DEFAULT 768,

    "rate_limit_per_min"    int NOT NULL DEFAULT 30,
    -- Ambient agents (opt-in). meeting_recap_enabled controls the
    -- post-call summary agent that turns a finished call's transcript into
    -- a recap (summary + decisions + action items) posted to the channel/
    -- DM where the call happened.
    "meeting_recap_enabled" boolean NOT NULL DEFAULT false,
    -- memory_layer_enabled controls the Workspace Memory extraction agent
    -- (migration 65): on each meeting recap (and, later, on demand) it
    -- extracts decisions / commitments / open questions into structured,
    -- queryable memory items.
    "memory_layer_enabled"  boolean NOT NULL DEFAULT false,
    "updated_at"            TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Seed the singleton row pointing at the built-in Ollama provider with
-- the historical defaults (llama3.2:3b + nomic-embed-text @ 768). This
-- preserves the previous out-of-the-box behaviour for fresh installs.
INSERT INTO ai_settings (
    "id", "enabled",
    "chat_provider_id", "chat_model",
    "embedding_provider_id", "embedding_model", "embedding_dimension",
    "rate_limit_per_min"
) VALUES (
    1, true,
    '00000000-0000-0000-0000-0000000000a1', 'llama3.2:3b',
    '00000000-0000-0000-0000-0000000000a1', 'nomic-embed-text', 768,
    30
)
ON CONFLICT ("id") DO NOTHING;
