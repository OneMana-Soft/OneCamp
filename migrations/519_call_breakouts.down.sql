DROP TABLE IF EXISTS call_breakout_assignments;
DROP TABLE IF EXISTS call_breakouts;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS breakouts_closing_at;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS breakouts_ends_at;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS breakouts_choose;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS breakouts_open_at;
-- A room's call goes with the rooms: it was its parent's.
DELETE FROM call_sessions WHERE kind = 'breakout';
ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_ended_reason_check;
ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_ended_reason_check
    CHECK (ended_reason IN ('last_left', 'room_finished', 'reconciled', 'host_ended'));
DROP INDEX IF EXISTS call_sessions_parent_idx;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS parent_session_id;
ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_kind_kind;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_sessions_kind_check') THEN
        ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_kind_check
            CHECK (kind IN ('huddle', 'meeting'));
    END IF;
END $$;
