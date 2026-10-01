-- Migration 92: API tokens — scoped, hashed personal access tokens for the
-- public API (Phase 5) and the MCP server endpoint (Phase 2.1, which depends on
-- these). A token authenticates as its creating user; every request through it
-- runs with that user's permissions, further narrowed by the token's scopes.
--
-- Only a SHA-256 hash of the token is stored; the plaintext is shown once at
-- creation and never again. A short non-secret prefix is kept for display
-- ("oc_live_ab12…") so users can recognize a token in the list.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS api_tokens (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"        varchar NOT NULL,                  -- user-facing label
    "token_hash"  varchar NOT NULL,                  -- sha256 hex of the secret
    "token_prefix" varchar NOT NULL,                 -- non-secret display prefix
    "scopes"      jsonb NOT NULL DEFAULT '[]'::jsonb, -- granted scope strings
    "created_by"  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "last_used_at" TIMESTAMP WITH TIME ZONE,
    "expires_at"  TIMESTAMP WITH TIME ZONE,          -- NULL = never expires
    "revoked_at"  TIMESTAMP WITH TIME ZONE,          -- NULL = active
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Auth hot path: look up an active token by its hash.
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens (token_hash);

CREATE INDEX IF NOT EXISTS idx_api_tokens_created_by
    ON api_tokens (created_by)
    WHERE revoked_at IS NULL;
