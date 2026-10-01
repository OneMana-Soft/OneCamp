-- Migration 75: AI Proactive Nudges.
--
-- Nudges are the "push" arm of the workspace AI: instead of waiting to be
-- asked, OneCamp periodically evaluates workspace state (overdue commitments,
-- stale open questions, blocked tasks, …) and surfaces a short, actionable
-- nudge to the responsible user — in-app in real time, optionally by email.
--
-- This is the feature that makes the product feel alive. It is opt-in
-- (ai_settings.nudges_enabled) and reuses the memory layer + connectors as its
-- signal source, so it pays nothing until both AI and nudges are enabled.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Admin toggle on the singleton AI settings row.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS nudges_enabled boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS workspace_nudges (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Recipient: the user this nudge is for.
    "user_id"       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- kind classifies the nudge so the UI can group/icon it and so dedup is
    -- scoped per kind. Keep aligned with the Go constants.
    "kind"          varchar NOT NULL
        CHECK (kind IN (
            'overdue_commitment', 'stale_question', 'blocked_task',
            'unreviewed_pr', 'idle_decision', 'generic'
        )),

    -- Human-facing copy (already AI-phrased or template-rendered).
    "title"         varchar NOT NULL,
    "body"          text NOT NULL DEFAULT '',

    -- Optional deep link target (e.g. /app/task/<uuid>, /app/ai/memory).
    "cta_url"       text,
    "cta_text"      varchar,

    -- Provenance: what the nudge was derived from, so acting on it can resolve
    -- the source (e.g. a memory item id or a task uuid). source_type ∈
    -- memory|task|connector|… ; source_id is that entity's id/uuid.
    "source_type"   varchar NOT NULL DEFAULT '',
    "source_id"     varchar NOT NULL DEFAULT '',

    -- Lifecycle. 'open' until the user dismisses or acts on it; superseded
    -- when a newer nudge for the same logical signal replaces it.
    "status"        varchar NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'dismissed', 'acted', 'superseded')),

    -- Priority drives ordering + visual weight: 0 = normal, 1 = high.
    "priority"      smallint NOT NULL DEFAULT 0,

    -- Idempotency: one live nudge per logical signal. The engine recomputes
    -- the same dedup_key each run; a partial unique index permits at most one
    -- non-terminal (open) row per key so re-evaluation upserts rather than
    -- duplicates, while keeping the full history of dismissed/acted rows.
    "dedup_key"     varchar NOT NULL,

    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- At most one OPEN nudge per logical signal.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_nudges_open_dedup
    ON workspace_nudges (dedup_key)
    WHERE status = 'open';

-- Hot path: a user's open nudges, newest/highest-priority first.
CREATE INDEX IF NOT EXISTS idx_workspace_nudges_user_open
    ON workspace_nudges (user_id, status, priority DESC, created_at DESC);

-- Housekeeping: purge old terminal nudges by age.
CREATE INDEX IF NOT EXISTS idx_workspace_nudges_status_created
    ON workspace_nudges (status, created_at);
