-- Migration 190: automatic check-ins. A question a channel asks its people on
-- chosen days at a time of day in a time zone ("What did you work on today?",
-- weekdays at 17:00). Each time, OneCamp posts it in the channel and everyone
-- answers in its thread. days is a bitmask, Monday 1 to Sunday 64; at_minute
-- is the local time of day. next_run_at is the next time it asks: a worker
-- claims a due time by moving it on in one UPDATE, so exactly one asks it.
CREATE TABLE IF NOT EXISTS checkins (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "channel_uuid"   uuid NOT NULL,
    "question"       varchar(300) NOT NULL CHECK (char_length(btrim(question)) BETWEEN 1 AND 300),
    "days"           smallint NOT NULL CHECK (days BETWEEN 1 AND 127),
    "at_minute"      smallint NOT NULL CHECK (at_minute BETWEEN 0 AND 1439),
    "tz"             varchar(64) NOT NULL,
    "created_by"     uuid NOT NULL,
    "paused"         boolean NOT NULL DEFAULT false,
    "next_run_at"    TIMESTAMP WITH TIME ZONE,
    "last_post_uuid" uuid,
    "last_asked_at"  TIMESTAMP WITH TIME ZONE,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"     TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS checkins_channel ON checkins (channel_uuid) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS checkins_due ON checkins (next_run_at) WHERE deleted_at IS NULL AND NOT paused;
