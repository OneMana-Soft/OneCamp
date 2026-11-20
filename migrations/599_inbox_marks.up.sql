-- Migration 599 (A4 H5, slack-04/05): a person's own marks on their Inbox
-- items, one row per person and item. The first mark: taking an item out of
-- Priority (dismissed_at; POST /activity/dismiss). The item is named by the
-- key the feed sends with it (business/Activity itemKey), an opaque string
-- that only ever matches inside the person's own feed. Putting it back clears
-- dismissed_at and keeps the row: nothing here is ever deleted. Every read
-- and write is the person's own (user_id is the session's); no admin, export
-- or audit route reads it. No foreign key, as saved_items.
CREATE TABLE IF NOT EXISTS inbox_marks (
    "user_id"      uuid NOT NULL,
    "item_key"     text NOT NULL CHECK (octet_length(item_key) <= 200),
    "dismissed_at" TIMESTAMP WITH TIME ZONE,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, item_key)
);
