-- Migration 243: drafts kept on the server (design/chat-vs-slack/PROGRAM.md
-- B24's drafts, D-M4; chat P5). The working number is the chat programme's;
-- the integrator gives it the next free number at merge.
--
-- A message someone has started and not sent, one per person and place: a
-- channel, a DM or a group, or a thread in one. It is theirs alone: nobody
-- else reads it, and it stays until they send it or empty the box (O17: no
-- automatic deletion). The place is named by its draft key
-- (business/Draft): "channel:<uuid>", "dm:<uuid> <uuid>", "group:<id>",
-- "post-thread:<root uuid>" or "chat-thread:<root uuid>".
--
-- reply_to is the message the draft answers inline ({uuid, author_name,
-- text}); files the attachments already uploaded for it, as the composer
-- holds them. user_id names the owner as business/DataSubject's inventory
-- finds personal data. Nothing here needs a uuid made by Postgres, so it all
-- runs on Postgres 12.
CREATE TABLE IF NOT EXISTS message_drafts (
    user_id    uuid        NOT NULL,
    draft_key  text        NOT NULL CHECK (length(draft_key) BETWEEN 3 AND 200),
    body_html  text        NOT NULL DEFAULT '' CHECK (length(body_html) <= 100000),
    reply_to   jsonb,
    files      jsonb,
    also_send  boolean     NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, draft_key)
);

-- A person's drafts, newest first: the one read the app makes.
CREATE INDEX IF NOT EXISTS idx_message_drafts_recent ON message_drafts (user_id, updated_at DESC);
