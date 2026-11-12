-- A board's polls have no place to live once polls has no board.
DELETE FROM polls WHERE board_uuid IS NOT NULL;
DROP INDEX IF EXISTS polls_board_idx;
ALTER TABLE polls DROP CONSTRAINT IF EXISTS polls_board_frame_check;
ALTER TABLE polls
    DROP COLUMN IF EXISTS frame_id,
    DROP COLUMN IF EXISTS board_uuid;
