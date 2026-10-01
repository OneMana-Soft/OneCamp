-- Migration 73: First-class, user-facing slash command registry + app
-- installations. Generalizes the webhook-scoped `webhook_commands` (migration
-- 46) into a workspace-wide catalog the composer can query, and reuses the
-- existing `integrations` table (migration 19) as the OAuth/token store for
-- installed apps (Giphy / Zoom / Jira / Google Calendar ...).

-- Installed apps. An app bundles commands + (optionally) an OAuth integration.
-- Token storage stays in `integrations` (entity_type='app', provider=app.slug);
-- this table is the catalog/registry + signing material for external dispatch.
CREATE TABLE IF NOT EXISTS apps (
    "id"             uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "slug"           varchar NOT NULL,            -- "giphy", "zoom", "jira", "google_calendar"
    "name"           varchar NOT NULL,            -- display name
    "description"    text,
    "icon_url"       text,
    -- builtin  → ships with OneCamp, no external calls (e.g. core command pack)
    -- external → dispatches commands to handler_url (Slack-compatible)
    -- oauth    → external + needs an OAuth install flow (token in integrations)
    "kind"           varchar NOT NULL DEFAULT 'external',
    -- HMAC signing secret for outbound command dispatch (X-OneCamp-Signature).
    "signing_secret" varchar,
    -- Default handler URL for the app's external commands (SSRF-guarded).
    "handler_url"    text,
    -- OAuth config (client id, scopes, auth/token URLs) when kind='oauth'.
    "oauth_config"   jsonb,
    -- Free-form per-app config (api keys entered by admin, feature flags).
    "config"         jsonb DEFAULT '{}'::jsonb,
    "is_enabled"     boolean NOT NULL DEFAULT true,
    "installed_by"   uuid REFERENCES users(id),
    "created_at"     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"     TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_apps_slug
    ON apps (slug) WHERE deleted_at IS NULL;

-- The command catalog. Built-in commands have app_id IS NULL.
CREATE TABLE IF NOT EXISTS slash_commands (
    "id"            uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- Stored WITHOUT the leading slash, lowercased: "remind", "giphy".
    "command"       varchar NOT NULL,
    "app_id"        uuid REFERENCES apps(id) ON DELETE CASCADE,
    "description"   text NOT NULL,
    "usage_hint"    varchar,                      -- "[search term]", "@who what when"
    -- inline | interactive | deferred | external
    "exec_mode"     varchar NOT NULL DEFAULT 'inline',
    -- For external apps: where to POST the command (overrides app.handler_url).
    "handler_url"   text,
    -- Scoping reuses the webhook convention: org | team | channel.
    "scope_type"    varchar NOT NULL DEFAULT 'org',
    "scope_entity_id" uuid,
    -- Whether the result is posted to the channel or shown only to the invoker.
    "response_type" varchar NOT NULL DEFAULT 'ephemeral', -- ephemeral | in_channel
    "is_builtin"    boolean NOT NULL DEFAULT false,
    "is_enabled"    boolean NOT NULL DEFAULT true,
    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"    TIMESTAMP WITH TIME ZONE
);

-- A command name must be unique within its scope (core command wins by being
-- org-scoped + is_builtin; apps that collide are namespaced at resolve time).
CREATE UNIQUE INDEX IF NOT EXISTS idx_slash_commands_unique
    ON slash_commands (command, scope_type, COALESCE(scope_entity_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_slash_commands_scope
    ON slash_commands (scope_type, scope_entity_id)
    WHERE deleted_at IS NULL AND is_enabled = true;

CREATE INDEX IF NOT EXISTS idx_slash_commands_app
    ON slash_commands (app_id) WHERE deleted_at IS NULL;
