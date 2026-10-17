-- Migration 210: follows, the inbox's notices, and who a body mentions (the
-- docs programme's A6: business/Follow, business/Notification, business/Doc's
-- mentions hook). Any kind of thing, not only docs: a channel, an event or a
-- recording takes the same rows.
--
-- resource_follows: how much a person chose to hear about one thing.
--   mode  'all'       every comment, reply, proposal and change worth telling;
--         'mentions'  replies to what they take part in, and @mentions of them;
--         'none'      only an @mention of them.
--   A row is a choice someone made. With no row, a doc's owner and editors
--   follow "replies and mentions", and everyone else hears only when they're
--   mentioned or take part (business/Follow's defaults), so nothing has to be
--   written when a doc is made or shared.
--
-- resource_notices: the inbox's items for what has no item of its own (a
--   mention in a doc's body, "shared with you", a proposal), one row per person
--   told. They carry ids, never a body or a title: the title is read when the
--   inbox is, and only for someone who can still open the thing.
--   dedup_key makes telling idempotent: a repeated event writes nothing more,
--   and only a written row is pushed or emailed.
--
-- resource_mentions: who a thing's body mentions now, so a save tells only the
--   people it newly mentions. A purge removes a thing's rows from all three.
--
-- ids are made in Go: Postgres 12 has no gen_random_uuid().
CREATE TABLE IF NOT EXISTS resource_follows (
    "user_uuid"     uuid NOT NULL,
    "resource_kind" varchar(32) NOT NULL,
    "resource_uuid" varchar(128) NOT NULL,
    "mode"          varchar(16) NOT NULL CHECK (mode IN ('all', 'mentions', 'none')),
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("user_uuid", "resource_kind", "resource_uuid")
);

-- A thing's followers, by mode ("everyone following all of this doc").
CREATE INDEX IF NOT EXISTS resource_follows_resource_idx
    ON resource_follows (resource_kind, resource_uuid, mode);

CREATE TABLE IF NOT EXISTS resource_notices (
    "id"            uuid PRIMARY KEY,
    "user_uuid"     uuid NOT NULL,
    "kind"          varchar(64) NOT NULL,
    "resource_kind" varchar(32) NOT NULL,
    "resource_uuid" varchar(128) NOT NULL,
    "actor_uuid"    uuid,
    "agent_uuid"    uuid,
    "reason"        varchar(16) NOT NULL,
    "data"          jsonb NOT NULL DEFAULT '{}'::jsonb,
    "dedup_key"     varchar(255) NOT NULL UNIQUE,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- A person's inbox, newest first.
CREATE INDEX IF NOT EXISTS resource_notices_user_idx
    ON resource_notices (user_uuid, created_at DESC);

-- Everything told about one thing, for when it's gone for good.
CREATE INDEX IF NOT EXISTS resource_notices_resource_idx
    ON resource_notices (resource_kind, resource_uuid);

-- The retention sweep's range.
CREATE INDEX IF NOT EXISTS resource_notices_created_idx
    ON resource_notices (created_at);

CREATE TABLE IF NOT EXISTS resource_mentions (
    "resource_kind" varchar(32) NOT NULL,
    "resource_uuid" varchar(128) NOT NULL,
    "user_uuid"     uuid NOT NULL,
    "mentioned_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("resource_kind", "resource_uuid", "user_uuid")
);
