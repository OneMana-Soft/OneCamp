-- Migration 85: Board snapshots (mass-delete recovery / version history).
--
-- The live board canvas is a collaborative Yjs document persisted to dgraph
-- (board_state). A destructive action (someone selecting-all and deleting, a
-- buggy client, a bad import) would otherwise overwrite that state with an
-- empty/smaller one and the prior content would be unrecoverable.
--
-- This table is the lightweight INDEX of point-in-time board snapshots; the
-- snapshot blob itself (gzipped base64 Yjs update) lives in object storage
-- (MinIO), keyed by object_key. Keeping blobs out of Postgres keeps the hot
-- DB lean while still giving us a queryable, prunable history.
--
-- Retention is bounded (see the cleanup loop + per-board cap), so storage
-- does not grow without limit: total ~= boards x snapshots-per-board x size.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS board_snapshots (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- The board this snapshot belongs to (dgraph board_uuid).
    "board_uuid"    varchar NOT NULL,

    -- MinIO object key holding the gzipped base64 Yjs state for this snapshot.
    "object_key"    varchar NOT NULL,

    -- Element count at snapshot time (parsed from the collab snippet) and the
    -- uncompressed base64 state size in bytes - both informational, surfaced
    -- in the version-history UI and used for mass-delete heuristics.
    "element_count" integer NOT NULL DEFAULT 0,
    "state_bytes"   integer NOT NULL DEFAULT 0,

    -- Why this snapshot was captured.
    --   interval    - periodic capture of the current state during activity
    --   mass_delete - the prior (larger) state, captured because the incoming
    --                 state shrank sharply (protects against accidental wipes)
    --   manual      - captured just before an explicit restore (so restore is
    --                 itself reversible)
    "reason"        varchar NOT NULL DEFAULT 'interval'
        CHECK (reason IN ('interval','mass_delete','manual')),

    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Hot path: list a board's snapshots newest-first, and find the latest for
-- cadence/mass-delete comparisons.
CREATE INDEX IF NOT EXISTS idx_board_snapshots_board_created
    ON board_snapshots (board_uuid, created_at DESC);

-- Age-based pruning sweeps by created_at across all boards.
CREATE INDEX IF NOT EXISTS idx_board_snapshots_created
    ON board_snapshots (created_at);
