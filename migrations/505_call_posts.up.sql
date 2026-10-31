-- A call's post (calls CALLS-3b). A live call's sessions are found by their
-- post (HU13's thread admission, the call's thread surface), and the ones
-- still without a post by the repair sweep (business/Huddle): one partial
-- index serves both, over live sessions only.
CREATE INDEX IF NOT EXISTS call_sessions_live_post ON call_sessions (post_uuid) WHERE ended_at IS NULL;

-- The repair's record: how many times the post's write failed, and when it
-- was refused for good (the starter may not post there), so the sweep moves
-- past both.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS post_tries smallint NOT NULL DEFAULT 0;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS post_refused_at timestamptz;
