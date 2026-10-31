ALTER TABLE call_sessions DROP COLUMN IF EXISTS post_refused_at;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS post_tries;
DROP INDEX IF EXISTS call_sessions_live_post;
