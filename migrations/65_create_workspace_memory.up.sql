-- Migration 65: Workspace Memory Layer.
--
-- The memory layer is OneCamp's compounding moat: as a workspace is used,
-- AI agents continuously extract durable, structured knowledge — decisions,
-- commitments, open questions, and glossary terms — from chat, docs, and
-- meeting transcripts, and write them here as first-class, queryable
-- entities. Unlike the vector index (semantic recall of raw content), this
-- is the STRUCTURED layer: deduplicated, status-tracked, owner-attributed,
-- and permission-scoped.
--
-- Permission model mirrors the rest of the app: an item is scoped to a
-- channel and/or project (and/or a DM grouping id). Retrieval filters by
-- the requesting user's accessible channels/projects — identical to the
-- AI embedding permission filter — so memory never leaks across access
-- boundaries.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS workspace_memory_items (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- kind classifies the extracted fact.
    "kind"          varchar NOT NULL
        CHECK (kind IN ('decision','commitment','question','glossary')),

    -- content is the normalized, self-contained statement of the fact
    -- (e.g. "Decided to ship the API redesign in Q3").
    "content"       text NOT NULL,

    -- status tracks lifecycle. Commitments/questions can be resolved;
    -- decisions are typically 'open' (active) until superseded.
    "status"        varchar NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','resolved','superseded','dismissed')),

    -- Optional owner (the user responsible / who committed).
    "owner_user_id" uuid REFERENCES users(id) ON DELETE SET NULL,

    -- Optional due date for commitments.
    "due_at"        TIMESTAMP WITH TIME ZONE,

    -- ── Permission scope (at least one should be set) ──────────────────
    -- channel_uuid / project_uuid mirror the embedding permission fields.
    -- chat_grp_id scopes DM/group-chat-derived items. team_uuid is a
    -- coarse fallback. NULLs mean "not scoped to that dimension".
    "channel_uuid"  uuid,
    "project_uuid"  uuid,
    "chat_grp_id"   varchar,
    "team_uuid"     uuid,

    -- ── Provenance ─────────────────────────────────────────────────────
    -- Where this fact came from, so the UI can link back and so re-runs
    -- can dedupe by source. source_type ∈ post|chat|doc|transcript|recap.
    "source_type"   varchar NOT NULL DEFAULT 'unknown',
    "source_uuid"   varchar,
    -- A stable hash of (kind + normalized content + scope) used for
    -- idempotent upserts so re-extraction of the same conversation does
    -- not create duplicate rows.
    "dedup_hash"    varchar NOT NULL,

    -- confidence is the extractor's self-reported 0-100 score; low-
    -- confidence items can be filtered out of high-signal surfaces.
    "confidence"    smallint NOT NULL DEFAULT 70
        CHECK (confidence BETWEEN 0 AND 100),

    "created_by_user_id" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    -- Soft delete so dismissals are reversible and auditable.
    "deleted_at"    TIMESTAMP WITH TIME ZONE
);

-- Idempotency: one row per logical fact. Re-extraction upserts.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_memory_dedup
    ON workspace_memory_items (dedup_hash)
    WHERE deleted_at IS NULL;

-- Retrieval indexes — the hot paths are "recent open items in my
-- accessible channels/projects" and "items by kind".
CREATE INDEX IF NOT EXISTS idx_workspace_memory_channel
    ON workspace_memory_items (channel_uuid, status, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_memory_project
    ON workspace_memory_items (project_uuid, status, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_memory_grp
    ON workspace_memory_items (chat_grp_id, status, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_memory_owner
    ON workspace_memory_items (owner_user_id, status)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_memory_kind
    ON workspace_memory_items (kind, status, created_at DESC)
    WHERE deleted_at IS NULL;
