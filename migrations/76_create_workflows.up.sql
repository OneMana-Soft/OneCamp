-- Migration 76: Workflow Builder — event-triggered automation.
--
-- Slack-parity "when X happens, do Y" automation, made native to OneCamp by
-- composing primitives that already exist: the workspace event bus
-- (webhook.DispatchEvent → "post.created", …) for TRIGGERS, and the business
-- layer (posts, tasks) for ACTIONS. No new delivery/queue machinery — a
-- workflow is just a saved rule the engine evaluates on each matching event.
--
-- Why this is sticky (not a "dumped" feature): teams wire one rule once
-- ("new message in #support → auto-acknowledge", "message with 'bug:' in
-- #eng → create a triage task") and the workspace quietly does the busywork
-- forever. It removes recurring manual toil, which is what makes automation
-- something a team can't go back to living without.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS workflows (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Human-facing name shown in the management UI.
    "name"          varchar NOT NULL,

    -- Disabled workflows are skipped by the engine but kept for editing.
    "is_active"     boolean NOT NULL DEFAULT true,

    -- Owner: actions run AS this user (their permissions are re-checked at
    -- execution time, so a workflow can never escalate privileges).
    "created_by"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- ── Trigger ──────────────────────────────────────────────────────────
    -- trigger_type drives which event the engine matches on. Kept as an open
    -- varchar with a CHECK so adding trigger kinds is a one-line migration.
    "trigger_type"  varchar NOT NULL DEFAULT 'message_posted'
        CHECK (trigger_type IN ('message_posted')),

    -- Optional channel scope. NULL = any channel the trigger fires in.
    "channel_id"    uuid,

    -- Keyword filter (JSON array of strings). Empty = fire on every message.
    "keywords"      jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- 'any' = match if ANY keyword present; 'all' = require all keywords.
    "match_type"    varchar NOT NULL DEFAULT 'any'
        CHECK (match_type IN ('any', 'all')),

    -- ── Actions ──────────────────────────────────────────────────────────
    -- Ordered JSON array of action objects, e.g.
    --   [{"type":"reply","text":"Thanks! We'll get back to you."},
    --    {"type":"create_task","project_id":"<uuid>","priority":"high"}]
    -- Validated in the business layer against the supported action schema.
    "actions"       jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- ── Bookkeeping ──────────────────────────────────────────────────────
    "run_count"     bigint NOT NULL DEFAULT 0,
    "last_run_at"   TIMESTAMP WITH TIME ZONE,
    "last_error"    text,

    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"    TIMESTAMP WITH TIME ZONE
);

-- Hot path: the engine loads "active workflows for this trigger type" on every
-- matching event. A partial index keeps that scan tiny and ignores soft-deleted
-- / inactive rows.
CREATE INDEX IF NOT EXISTS idx_workflows_active_trigger
    ON workflows (trigger_type)
    WHERE is_active = true AND deleted_at IS NULL;

-- For channel-scoped lookups and the management list.
CREATE INDEX IF NOT EXISTS idx_workflows_channel
    ON workflows (channel_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_workflows_created_by
    ON workflows (created_by)
    WHERE deleted_at IS NULL;
