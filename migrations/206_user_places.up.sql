-- Migration 206: places, a person's stars and pins, of any kind of thing.
--
-- One table for every kind, so a starred doc, channel, calendar or recording
-- can sit in one list in the order its person put them (business/Places).
--
--   kind           what the row is: a star, or a pin.
--   resource_kind  what it points at ("doc"; other kinds register a resolver).
--   resource_uuid  that thing's id. Text, not uuid: some ids aren't uuids (a
--                  recording is named by its egress id).
--   position       where it sits in its person's list of that kind: a
--                  fractional key (business/Tree's), compared byte by byte,
--                  hence COLLATE "C". Moving a row rewrites that row only.
--
-- A person stars a thing once: (user_uuid, kind, resource_uuid) is unique.
-- Recents aren't stored here: they come from resource_views, which already
-- records every open of a doc or a board.
--
-- ids are made in Go: Postgres 12 has no gen_random_uuid().
CREATE TABLE IF NOT EXISTS user_places (
    "id"            uuid PRIMARY KEY,
    "user_uuid"     uuid NOT NULL,
    "kind"          varchar(16) NOT NULL CHECK (kind IN ('star', 'pin')),
    "resource_kind" varchar(32) NOT NULL,
    "resource_uuid" varchar(128) NOT NULL,
    "position"      varchar(64) COLLATE "C" NOT NULL,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE ("user_uuid", "kind", "resource_uuid")
);

-- A person's list of one kind, in its order.
CREATE INDEX IF NOT EXISTS user_places_list_idx
    ON user_places (user_uuid, kind, position);

-- Everyone's places on one thing, for when it's gone for good.
CREATE INDEX IF NOT EXISTS user_places_resource_idx
    ON user_places (resource_kind, resource_uuid);

-- Recent docs: a person's own views, newest first. resource_views was only
-- indexed by resource ("who viewed this"), so the list read the whole table.
CREATE INDEX IF NOT EXISTS resource_views_user_recent_idx
    ON resource_views (user_uuid, resource_type, last_viewed_at DESC);
