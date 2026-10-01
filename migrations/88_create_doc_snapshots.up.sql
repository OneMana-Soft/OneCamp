-- Migration 88: doc snapshots (version history / mass-delete recovery).
--
-- Mirrors board_snapshots (migrations 85-86) for documents. The live doc body
-- (HTML) is persisted to dgraph on every collab save; without history, a
-- select-all-delete or bad paste overwrites it irrecoverably. This gives docs
-- the same recoverable version history boards have.
--
-- Hybrid storage: this Postgres table is the lightweight INDEX; the snapshot
-- blob (gzipped HTML body) lives in object storage (MinIO), keyed by
-- object_key. Retention is bounded by a per-doc cap + an age sweep so storage
-- never grows without limit.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS doc_snapshots (
    "id"               uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- The document this snapshot belongs to (dgraph doc_uuid).
    "doc_uuid"         varchar NOT NULL,

    -- MinIO object key holding the gzipped HTML body for this snapshot.
    "object_key"       varchar NOT NULL,

    -- Uncompressed body size in bytes - informational + used by the
    -- mass-delete (sharp shrink) heuristic.
    "body_bytes"       integer NOT NULL DEFAULT 0,

    -- Why this snapshot was captured (interval | mass_delete | manual).
    "reason"           varchar NOT NULL DEFAULT 'interval'
        CHECK (reason IN ('interval','mass_delete','manual')),

    -- The set of users who edited the doc in the window leading up to this
    -- snapshot (version "edited by"). May be empty.
    "contributor_uuids" text[] NOT NULL DEFAULT '{}',

    "created_at"       TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doc_snapshots_doc_created
    ON doc_snapshots (doc_uuid, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_doc_snapshots_created
    ON doc_snapshots (created_at);
