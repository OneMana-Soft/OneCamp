DROP TABLE IF EXISTS call_question_votes;
DROP TABLE IF EXISTS call_questions;

ALTER TABLE call_sessions
    DROP COLUMN IF EXISTS report_done_at,
    DROP COLUMN IF EXISTS report_claimed_at,
    DROP COLUMN IF EXISTS qa_anonymous,
    DROP COLUMN IF EXISTS qa_shown,
    DROP COLUMN IF EXISTS qa_open;

-- A call's polls have no place to live once polls has no session.
DELETE FROM polls WHERE session_id IS NOT NULL;
DROP INDEX IF EXISTS polls_session_idx;
ALTER TABLE polls
    DROP COLUMN IF EXISTS results_hidden,
    DROP COLUMN IF EXISTS anonymous,
    DROP COLUMN IF EXISTS session_id;
