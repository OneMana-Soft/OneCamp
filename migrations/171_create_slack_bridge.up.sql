-- Migration 171: a live bridge to one Slack workspace.
--
-- A team moving from Slack rarely moves in one day. The bridge lets both sides
-- keep talking while it does: a message in a linked Slack channel appears in
-- its OneCamp channel under the sender's name, and a OneCamp message appears in
-- Slack under its author's name. Slack people take no OneCamp seat.
--
-- One Slack workspace per OneCamp workspace: the row id is pinned to 1.
CREATE TABLE IF NOT EXISTS slack_bridge (
    id                 smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    team_id            text        NOT NULL,
    team_name          text        NOT NULL DEFAULT '',
    bot_user_id        text        NOT NULL DEFAULT '',
    bot_id             text        NOT NULL DEFAULT '',
    -- Both secrets are sealed with helpers.EncryptSecret (APP_SECRET_KEK).
    bot_token_enc      text        NOT NULL,
    signing_secret_enc text        NOT NULL,
    created_by         uuid,
    created_at         timestamptz NOT NULL DEFAULT NOW(),
    updated_at         timestamptz NOT NULL DEFAULT NOW(),
    -- The last delivery failure, shown to the admin so a revoked token or a
    -- channel the app was removed from is not a silent outage.
    last_error         text        NOT NULL DEFAULT '',
    last_error_at      timestamptz
);

-- A Slack channel and a OneCamp channel carrying the same conversation. Each
-- side belongs to at most one link, so a message has exactly one destination.
CREATE TABLE IF NOT EXISTS slack_bridge_links (
    id                 uuid PRIMARY KEY,
    slack_channel_id   text        NOT NULL UNIQUE,
    slack_channel_name text        NOT NULL DEFAULT '',
    channel_uuid       uuid        NOT NULL UNIQUE,
    created_by         uuid,
    created_at         timestamptz NOT NULL DEFAULT NOW()
);

-- Which OneCamp post or comment each bridged Slack message is. Needed to
-- thread replies and to carry edits and deletions across. origin says which
-- side wrote it: an edit only ever travels away from its origin, which is what
-- stops a bridged message echoing back and forth.
--
-- The (slack_channel_id, slack_ts) key doubles as the claim that makes a
-- retried Slack delivery a no-op: post_uuid is NULL until the post exists.
CREATE TABLE IF NOT EXISTS slack_bridge_messages (
    slack_channel_id text        NOT NULL,
    slack_ts         text        NOT NULL,
    channel_uuid     uuid        NOT NULL,
    post_uuid        uuid,
    comment_uuid     uuid,
    origin           text        NOT NULL CHECK (origin IN ('slack', 'onecamp')),
    created_at       timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (slack_channel_id, slack_ts)
);
CREATE INDEX IF NOT EXISTS slack_bridge_messages_post_idx ON slack_bridge_messages (post_uuid);
