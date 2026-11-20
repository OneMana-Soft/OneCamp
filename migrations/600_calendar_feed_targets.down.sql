-- Back to links of one calendar only: every live workspace link stops (a
-- stop, not a delete), so nothing reads a row with no calendar. calendar_id
-- stays nullable, as those stopped rows keep no calendar.
UPDATE calendar_feeds SET revoked_at = NOW() WHERE calendar_id IS NULL AND revoked_at IS NULL;
DROP INDEX IF EXISTS calendar_feeds_live_made_idx;
DROP INDEX IF EXISTS calendar_feeds_workspace_live_idx;
ALTER TABLE calendar_feeds DROP COLUMN IF EXISTS last_read_at;
DROP INDEX IF EXISTS calendar_feeds_revoked_by_idx;
ALTER TABLE calendar_feeds DROP COLUMN IF EXISTS revoked_by;
