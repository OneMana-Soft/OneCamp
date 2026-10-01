-- Save for later: anything a member wants to come back to (a message, a task,
-- a doc), with an optional time to be reminded. Private to the member.
--
-- item_type + item_id name the thing; link is the app path that opens it and
-- title a short preview, both captured when it was saved so the list renders
-- without a lookup per row and still reads if the thing is later deleted.
--
-- remind_at is when it should bubble up; reminded_at when it last did.
-- done_at marks it finished; it stays listed under Done until removed.
CREATE TABLE IF NOT EXISTS saved_items (
    "id"          uuid PRIMARY KEY,
    "user_id"     uuid NOT NULL,
    "item_type"   varchar(32) NOT NULL,
    "item_id"     varchar(128) NOT NULL,
    "link"        varchar(512) NOT NULL,
    "title"       varchar(300) NOT NULL DEFAULT '',
    "context"     varchar(200) NOT NULL DEFAULT '',
    "remind_at"   TIMESTAMP WITH TIME ZONE,
    "reminded_at" TIMESTAMP WITH TIME ZONE,
    "done_at"     TIMESTAMP WITH TIME ZONE,
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE ("user_id", "item_type", "item_id")
);

CREATE INDEX IF NOT EXISTS saved_items_user_open_idx ON saved_items (user_id, created_at DESC) WHERE done_at IS NULL;
