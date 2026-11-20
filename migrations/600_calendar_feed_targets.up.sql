-- basecamp-05: a person's link to everything they may see (the workspace
-- feed) is a calendar_feeds row with no calendar: every calendar they may see
-- when it is read, each shown as its own link shows it, and the events they
-- are on elsewhere. One live one per person, as for a calendar. revoked_by is
-- who stopped a link (its person, or a workspace admin), so its person can
-- see who stopped it. Rows are never deleted: a stop sets revoked_at.
--
-- Postgres 12: every statement can run twice.

ALTER TABLE calendar_feeds ALTER COLUMN calendar_id DROP NOT NULL;
ALTER TABLE calendar_feeds ADD COLUMN IF NOT EXISTS revoked_by uuid REFERENCES users(id) ON DELETE SET NULL;
-- revoked_by is a foreign key: an index for ON DELETE SET NULL, live links
-- (no stopper) left out.
CREATE INDEX IF NOT EXISTS calendar_feeds_revoked_by_idx ON calendar_feeds (revoked_by) WHERE revoked_by IS NOT NULL;
-- When a link was last read, at most once an hour (feeds.go feedReadEvery),
-- so an admin can tell a link in use from one nobody reads.
ALTER TABLE calendar_feeds ADD COLUMN IF NOT EXISTS last_read_at timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS calendar_feeds_workspace_live_idx ON calendar_feeds (person_id) WHERE calendar_id IS NULL AND revoked_at IS NULL;
-- An admin's list of live links, newest first.
CREATE INDEX IF NOT EXISTS calendar_feeds_live_made_idx ON calendar_feeds (created_at DESC) WHERE revoked_at IS NULL;
