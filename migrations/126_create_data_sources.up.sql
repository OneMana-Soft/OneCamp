-- Migration 126: external data sources (read-only warehouse/DB connectors).
--
-- Adds admin-managed, READ-ONLY connections to external SQL databases (Postgres
-- to start) so an agent/assistant can answer questions from a warehouse the way
-- it already answers from native Tables — without OneCamp ever holding a second
-- copy of that data. This is the governed complement to the in-app Tables
-- aggregation engine: the same deterministic, typed query plan is pushed down to
-- the external database; the model never writes raw SQL.
--
-- Security / governance properties (enforced in the business + connector layers):
--   - Configuration is CAPABILITY-GATED (agent.manage: admins always, members
--     only when an admin opens the capability), exactly like MCP servers — the
--     other 'external endpoint the AI calls, with a stored secret' surface.
--   - The connection PASSWORD is encrypted at rest (helpers.EncryptSecret,
--     AES-256-GCM, KEK from APP_SECRET_KEK) and NEVER serialized to the FE.
--   - Connections are opened READ-ONLY (read-only transaction + statement
--     timeout + row cap) against a least-privilege user the operator provides.
--   - visibility governs who may QUERY a source, mirroring Tables exactly, and
--     is enforced PER USER (the agent runs AS the user and is scoped the same):
--       private   -> only the creator + admins (the safe default: an external
--                    connection authenticates with ONE stored credential and so
--                    cannot do per-user row security at the external DB, so a
--                    source is not shared workspace-wide unless deliberately set)
--       workspace -> any member
--   - enabled lets an operator pause a source without deleting its config.
--
-- Nothing changes for existing installs until an admin adds a source.
-- Idempotent.

CREATE TABLE IF NOT EXISTS data_sources (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    engine        text        NOT NULL DEFAULT 'postgres'
                              CHECK (engine IN ('postgres')),
    host          text        NOT NULL DEFAULT '',
    port          int         NOT NULL DEFAULT 5432,
    database_name text        NOT NULL DEFAULT '',
    username      text        NOT NULL DEFAULT '',
    -- Base64 AES-256-GCM ciphertext of the connection password (helpers.EncryptSecret).
    -- NULL/'' means no password configured. Never returned to the FE.
    password_enc  text,
    ssl_mode      text        NOT NULL DEFAULT 'require'
                              CHECK (ssl_mode IN ('disable','require','verify-ca','verify-full')),
    visibility    text        NOT NULL DEFAULT 'private'
                              CHECK (visibility IN ('private','workspace')),
    enabled       boolean     NOT NULL DEFAULT true,
    created_by    uuid        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT NOW(),
    updated_at    timestamptz NOT NULL DEFAULT NOW(),
    deleted_at    timestamptz
);

-- Hot lookups: list non-deleted sources; resolve a source's visibility/owner.
CREATE INDEX IF NOT EXISTS idx_data_sources_active     ON data_sources (deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_data_sources_created_by ON data_sources (created_by);
