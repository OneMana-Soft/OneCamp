-- linear-12 part 4: a person's secret calendar link to a project's cycles,
-- which Google Calendar, Apple Calendar or Outlook subscribes to. As a
-- calendar's feed (480_calendar_feeds): only the token's SHA-256 is kept, as
-- hex, so the link is shown once when made; making another revokes the old
-- one at once, and a revoked or unknown link answers nothing.
--
-- Postgres 12: ids come from the application; every statement can run twice.

CREATE TABLE IF NOT EXISTS cycle_feeds (
    "id"           uuid PRIMARY KEY NOT NULL,
    "project_uuid" uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "person_id"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "token_hash"   varchar(64) NOT NULL,
    "created_at"   timestamptz NOT NULL DEFAULT NOW(),
    "revoked_at"   timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS cycle_feeds_token_idx ON cycle_feeds (token_hash);
-- One live link per person and project.
CREATE UNIQUE INDEX IF NOT EXISTS cycle_feeds_live_idx ON cycle_feeds (project_uuid, person_id) WHERE revoked_at IS NULL;
-- Deleting a person finds their links, revoked ones too.
CREATE INDEX IF NOT EXISTS cycle_feeds_person_idx ON cycle_feeds (person_id);
