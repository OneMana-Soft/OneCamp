-- Migration 87: resource view tracking ("Viewed by", like Google Docs).
--
-- Records who has viewed a doc or board. Modeled exactly like Google's Activity
-- Dashboard viewer list: ONE row per (resource, user) showing their most recent
-- view, not an event log. The UNIQUE constraint makes dedup structural, so a
-- user refreshing the page can never create duplicate viewer entries.
--
-- A single generic table serves both docs and boards (resource_type), so there
-- is one model and one code path. Storage is bounded by distinct viewers per
-- resource (team size), not by view events, so it does not grow without limit.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS resource_views (
    "id"              uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- What was viewed. resource_type ∈ doc|board; resource_uuid is the dgraph
    -- doc_uuid / board_uuid. user_uuid is the dgraph user uuid (== JWT sub).
    "resource_type"   varchar NOT NULL CHECK (resource_type IN ('doc','board')),
    "resource_uuid"   varchar NOT NULL,
    "user_uuid"       varchar NOT NULL,

    -- first_viewed_at is immutable; last_viewed_at is bumped on later views
    -- (throttled in the model so refresh storms do not amplify writes).
    "first_viewed_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "last_viewed_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),

    -- One row per viewer per resource: dedup is structural.
    UNIQUE (resource_type, resource_uuid, user_uuid)
);

-- Hot path: list a resource's viewers most-recent-first.
CREATE INDEX IF NOT EXISTS idx_resource_views_resource
    ON resource_views (resource_type, resource_uuid, last_viewed_at DESC);
