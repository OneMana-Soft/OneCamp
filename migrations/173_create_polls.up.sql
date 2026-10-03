-- Migration 173: polls in channels.
--
-- A poll is a message with a live block in it. The message carries the question
-- as text (so notifications, search and the Slack bridge have something to say)
-- and a <div data-type="poll" data-id="..."> the web app renders as the vote
-- buttons; the poll itself, its options and its votes, live here.
--
-- Options are a small fixed list chosen when the poll is made, so they are a
-- jsonb array of {id, text} rather than a table: a vote references an option by
-- its id, and an option is never edited after anyone could have voted on it.
CREATE TABLE IF NOT EXISTS polls (
    id          uuid PRIMARY KEY,
    channel_id  uuid        NOT NULL,
    post_id     uuid,
    question    text        NOT NULL CHECK (char_length(question) BETWEEN 1 AND 300),
    options     jsonb       NOT NULL CHECK (jsonb_typeof(options) = 'array'),
    multiple    boolean     NOT NULL DEFAULT false,
    closes_at   timestamptz,
    closed_at   timestamptz,
    created_by  uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS polls_channel_idx ON polls (channel_id, created_at DESC);

-- One row per option a person chose. Re-voting replaces their rows in one
-- transaction, so a person can never hold two choices in a single-choice poll.
CREATE TABLE IF NOT EXISTS poll_votes (
    poll_id    uuid        NOT NULL REFERENCES polls (id) ON DELETE CASCADE,
    option_id  text        NOT NULL,
    user_id    uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (poll_id, user_id, option_id)
);

CREATE INDEX IF NOT EXISTS poll_votes_poll_idx ON poll_votes (poll_id);
