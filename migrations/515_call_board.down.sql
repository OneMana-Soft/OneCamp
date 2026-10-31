-- A snapshot kept as guests started drawing stays, as one taken by hand.
UPDATE board_snapshots SET reason = 'manual' WHERE reason = 'guests';
ALTER TABLE board_snapshots DROP CONSTRAINT IF EXISTS board_snapshots_reason_kind;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_snapshots_reason_check') THEN
        ALTER TABLE board_snapshots ADD CONSTRAINT board_snapshots_reason_check
            CHECK (reason IN ('interval', 'mass_delete', 'manual'));
    END IF;
END $$;

DROP TABLE IF EXISTS call_session_boards;
DROP INDEX IF EXISTS call_sessions_live_board_idx;
ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_board_draw_kind;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS board_guests_kept;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS board_guests_draw;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS board_draw;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS board_by;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS board_uuid;
