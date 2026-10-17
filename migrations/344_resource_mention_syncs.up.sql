-- Migration 344: a doc's first keeping of its mentions is a baseline.
--
-- Migration 210 keeps who a thing's body mentions (resource_mentions), so that
-- a save tells only the people it newly mentions (business/Doc's mentions
-- hook). It starts empty, so on the upgrade that brings it, the first save of
-- every doc that already mentions people would tell all of them, by push and
-- by email, as if they had just been mentioned. These two tables let the hook
-- tell a doc's first keeping from the ones after it.
--
-- resource_mention_syncs: the things whose mentions have been kept at least
--   once, and since when. A thing's first keeping writes its row.
-- resource_mention_tracking: since when each kind's mentions are kept. A doc
--   made before then and kept for the first time tells nobody: what its body
--   mentions then was written before anyone could be told this way. A doc
--   made since is kept from its first save, and tells as it always has.
CREATE TABLE IF NOT EXISTS resource_mention_syncs (
    "resource_kind" varchar(32) NOT NULL,
    "resource_uuid" varchar(128) NOT NULL,
    "synced_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("resource_kind", "resource_uuid")
);

CREATE TABLE IF NOT EXISTS resource_mention_tracking (
    "resource_kind" varchar(32) PRIMARY KEY,
    "since"         TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Docs' mentions are kept since the first one 210 kept, or from now: a
-- database upgraded from before 210 has none.
INSERT INTO resource_mention_tracking (resource_kind, since)
SELECT 'doc', COALESCE(MIN(mentioned_at), NOW()) FROM resource_mentions WHERE resource_kind = 'doc'
ON CONFLICT (resource_kind) DO NOTHING;

-- What 210 has kept already counts as kept.
INSERT INTO resource_mention_syncs (resource_kind, resource_uuid, synced_at)
SELECT resource_kind, resource_uuid, MIN(mentioned_at) FROM resource_mentions
GROUP BY resource_kind, resource_uuid
ON CONFLICT (resource_kind, resource_uuid) DO NOTHING;
