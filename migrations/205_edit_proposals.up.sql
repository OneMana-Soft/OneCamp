-- Migration 205: edit proposals (docs programme A9, PROGRAM.md §4 #19, D-10).
--
-- A proposed change to a resource that someone reviews before it applies: a
-- teammate's suggestion, an agent's, an AI rewrite kept for later. The change is
-- held here, off the doc, so a commenter's connection to the live doc stays
-- read-only; accepting applies it to the live doc as one Yjs transaction.
--
-- The anchor columns match threads (migration 210): where in the resource the
-- change sits, as Yjs relative positions while the doc keeps its lineage. base
-- is the content the change was made against and base_hash its hash; proposed
-- is the replacement; hunks are its reviewable pieces, each with its own
-- decision. pending is the step in flight (prepare, apply, insert below), held
-- while the collaboration service runs it, so two reviewers can never apply one
-- proposal twice.
--
-- Ids are made by the API (Postgres 12 has no gen_random_uuid without pgcrypto).
CREATE TABLE IF NOT EXISTS edit_proposals (
    id              uuid PRIMARY KEY,
    resource_kind   varchar(32) NOT NULL,
    resource_uuid   uuid        NOT NULL,
    thread_id       uuid,
    anchor_kind     varchar(16) NOT NULL DEFAULT 'none'
                    CHECK (anchor_kind IN ('yrange', 'block', 'message', 'time', 'cell', 'none')),
    anchor_start    bytea,
    anchor_end      bytea,
    anchor_block_id text,
    anchor_lineage  text,
    anchor_quote    text,
    anchor_meta     jsonb,
    base_hash       text        NOT NULL DEFAULT '',
    base            jsonb,
    proposed        jsonb       NOT NULL,
    hunks           jsonb       NOT NULL DEFAULT '[]',
    note            text        CHECK (note IS NULL OR char_length(note) <= 500),
    author_kind     varchar(16) NOT NULL CHECK (author_kind IN ('person', 'agent', 'ai')),
    author_uuid     uuid        NOT NULL,
    asker_uuid      uuid,
    run_id          text,
    status          varchar(16) NOT NULL DEFAULT 'open'
                    CHECK (status IN ('preparing', 'open', 'accepted', 'partial', 'declined', 'stale', 'withdrawn')),
    pending         jsonb,
    pending_at      timestamptz,
    applied_mode    varchar(16) CHECK (applied_mode IS NULL OR applied_mode IN ('replace', 'insert_below')),
    decided_by      uuid,
    decided_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW()
);

-- A doc's suggestions, open ones first in every read.
CREATE INDEX IF NOT EXISTS edit_proposals_resource ON edit_proposals (resource_kind, resource_uuid, status, created_at DESC);
-- An agent run's proposals ("what did Release Captain suggest?").
CREATE INDEX IF NOT EXISTS edit_proposals_run ON edit_proposals (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS edit_proposals_thread ON edit_proposals (thread_id) WHERE thread_id IS NOT NULL;
-- The sweep: proposals left preparing, and steps left pending, by a crash.
CREATE INDEX IF NOT EXISTS edit_proposals_pending ON edit_proposals (pending_at) WHERE pending_at IS NOT NULL;
