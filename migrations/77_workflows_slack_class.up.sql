-- Migration 77: Workflow Builder → Slack-class + shared Automation bot identity.
--
-- Three coordinated changes, all additive and safe for existing rows:
--
-- 1. Shared bot identity (the Slack-class foundation)
--    A workspace gets ONE synthetic "Automation" user (is_bot = true) that
--    BOTH webhooks and workflows post through, instead of silently posting as
--    the human creator. This gives automated messages a real, recognizable
--    identity (own name + avatar) and a single audit principal — mirroring
--    Slack's Bot User model, where individual integrations may still override
--    the per-message display name/icon on top of the shared bot.
--
--    Modeled exactly like external/ghost users (is_external): a real row in
--    Postgres + a Dgraph node, but it can never authenticate (no password,
--    SSO-managed paths reject it) and is hidden from member pickers.
--
-- 2. Richer workflow triggers
--    trigger_type widens to the Slack-class event set; trigger_config jsonb
--    carries per-trigger params (reaction emoji, target task status, …).
--
-- 3. Richer workflow actions need no column change — actions is already an
--    open jsonb schema validated in the business layer.

-- ── 1. Bot identity on users ───────────────────────────────────────────────
-- The shared automation bot is a real user row flagged is_bot. There can be at
-- most one workspace-level system bot: it is pinned to a reserved sentinel
-- email, and the users.email_id column already carries a UNIQUE constraint, so
-- the singleton guarantee + idempotent seeding (INSERT ... ON CONFLICT
-- (email_id)) come for free without an extra index.
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_bot boolean NOT NULL DEFAULT false;

-- ── 2. Workflow bot label + triggers ───────────────────────────────────────
-- Per-workflow display-name override layered on top of the shared bot
-- (Slack-style). NULL/empty → fall back to the workflow name, then the shared
-- bot's own name.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS bot_name varchar;

-- Per-trigger parameters as JSON (e.g. {"emoji":"eyes"} for reaction_added,
-- {"to_status":"done"} for task_status_changed). Empty object = no extra
-- constraints beyond trigger_type + channel scope.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS trigger_config jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Widen the allowed trigger kinds. Replacing the single-value CHECK with the
-- Slack-class set; keeping it a CHECK (not an enum) makes adding a kind a
-- one-line migration.
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed'
    ));
