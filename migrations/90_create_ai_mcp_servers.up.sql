-- Migration 90: MCP (Model Context Protocol) servers — external tool providers.
--
-- An admin registers an external MCP server (an HTTP endpoint exposing tools
-- via the MCP spec). On connect we introspect its tools (tools/list) and
-- register them into the SAME tool registry the AI executors / Agent Builder
-- use, namespaced by a per-server prefix. Agents can then call those tools
-- exactly like native ones — every call still flows through the registry's
-- authorization surface, and MCP calls are additionally bounded by the agent's
-- allow-list, scope, and step budget.
--
-- Secrets (bearer token / header value) are encrypted at the application layer
-- with AI_CONFIG_KEK (same KEK as AI provider keys) before they touch the DB.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS ai_mcp_servers (
    "id"                   uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Human-facing identity shown in the admin UI.
    "name"                 varchar NOT NULL,
    "description"          text,

    -- Connection. We speak MCP over Streamable HTTP; transport is kept as an
    -- open varchar + CHECK so adding a kind later is a one-line migration.
    "url"                  varchar NOT NULL,
    "transport"            varchar NOT NULL DEFAULT 'http'
        CHECK (transport IN ('http')),

    -- Authentication to the external server. The secret is stored encrypted
    -- (never in plaintext); auth_header_name is used only for the 'header' kind.
    "auth_type"            varchar NOT NULL DEFAULT 'none'
        CHECK (auth_type IN ('none', 'bearer', 'header')),
    "auth_header_name"     varchar,
    "auth_secret_encrypted" bytea,

    -- Disabled servers are not introspected and their tools are not registered.
    "enabled"              boolean NOT NULL DEFAULT true,

    -- Namespace prefix applied to this server's tool names in the registry
    -- (e.g. "mcp_github_"), so two servers exposing a "search" tool never
    -- collide. Unique across live servers.
    "tool_prefix"          varchar NOT NULL,

    -- Last-introspected tool list (jsonb array of {name, description, schema}),
    -- cached so the admin UI and registry rebuild do not require a live call.
    "tools_cache"          jsonb NOT NULL DEFAULT '[]'::jsonb,
    "last_introspected_at" TIMESTAMP WITH TIME ZONE,
    "last_error"           text,

    -- Who registered it (kept for audit; survives user deletion).
    "created_by"           uuid REFERENCES users(id) ON DELETE SET NULL,

    "created_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"           TIMESTAMP WITH TIME ZONE
);

-- Tool prefix must be unique among live servers so registered names never clash.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_mcp_servers_tool_prefix
    ON ai_mcp_servers (tool_prefix)
    WHERE deleted_at IS NULL;

-- Hot path: the registry rebuild loads "enabled servers".
CREATE INDEX IF NOT EXISTS idx_ai_mcp_servers_enabled
    ON ai_mcp_servers (enabled)
    WHERE deleted_at IS NULL;
