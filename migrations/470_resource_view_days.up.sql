-- Migration 470: a doc's views by day (docs C6-3).
--
-- resource_views (migration 87) keeps one row per member and resource, their
-- last view: who has seen it. This keeps how many, by day, for a doc's
-- insights: one row per resource, day, source and viewer, so a day's viewers
-- are its rows and its views their sum. Bounded by viewers a day; rows older
-- than 400 days are swept (business/ResourceView retention).
--
-- viewer_key is a member's uuid, or, for a published page's visitor, an HMAC
-- of their address and browser under a random salt that changes every day
-- and is never stored: no address is kept, and a visitor can't be followed
-- from one day to the next. A request with Global Privacy Control or Do Not
-- Track, or from a bot, is never counted.
CREATE TABLE IF NOT EXISTS resource_view_days (
    resource_type varchar NOT NULL CHECK (resource_type IN ('doc', 'board')),
    resource_uuid varchar NOT NULL,
    day           date NOT NULL,
    source        varchar(8) NOT NULL CHECK (source IN ('member', 'page')),
    viewer_key    varchar(64) NOT NULL,
    views         integer NOT NULL DEFAULT 1,
    last_at       timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (resource_type, resource_uuid, day, source, viewer_key)
);
CREATE INDEX IF NOT EXISTS resource_view_days_day ON resource_view_days (day);

-- "Don't record my views": a member's own choice, beside read receipts. Off,
-- neither resource_views nor resource_view_days hears of their views.
ALTER TABLE users_notification_preferences ADD COLUMN IF NOT EXISTS record_views BOOLEAN NOT NULL DEFAULT TRUE;

-- A doc's changes, for its insights: the record's "docs" rows name the doc in
-- their metadata (business/Follow audit.go). This reads one doc's newest
-- without walking the whole log, as migration 213 does for its AI rows.
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_doc_events
    ON admin_audit_log ((metadata ->> 'resource_uuid'), seq DESC)
    WHERE category = 'docs';
