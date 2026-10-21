-- Migration 245: a person's notification keywords and VIPs (design/chat-vs-
-- slack/PROGRAM.md B5, chat P3 stage 3). The working number is the chat
-- programme's; the integrator gives it the next free number at merge.
--
-- A keyword tells its person of a channel message that holds it as a whole
-- word, any case (helpers.KeywordPattern), as a mention would. A VIP's
-- messages and mentions reach their person through a pause, focus time and
-- quiet hours. Each is a person's own and goes with them: rows go when the
-- person does. Constant defaults and no uuids made by Postgres (Postgres 12).
CREATE TABLE IF NOT EXISTS user_notification_keywords (
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    keyword    varchar(64) NOT NULL,
    -- The order the person keeps them in (each change replaces the list).
    position   smallint    NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, keyword)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_notification_keywords_lower
    ON user_notification_keywords (user_id, lower(keyword));

CREATE TABLE IF NOT EXISTS user_vips (
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vip_user_id uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    position    smallint    NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, vip_user_id),
    CHECK (user_id <> vip_user_id)
);
-- Who has someone as a VIP: asked as their message is delivered.
CREATE INDEX IF NOT EXISTS idx_user_vips_vip ON user_vips (vip_user_id);
