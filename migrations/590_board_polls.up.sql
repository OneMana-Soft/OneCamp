-- Migration 590: polls on a slide (miro-28 to_win 4; business/Poll board.go).
-- A board's poll is one of business/Poll's polls with a board and a frame on
-- it, and neither a channel (the nil uuid, as a call's poll), a post nor a
-- call; its votes are poll_votes rows like any poll's. Who may start, read,
-- vote on and close it is the board's to decide (business/Board CanEdit and
-- CanRead). The board lives in Dgraph, so there is no foreign key. The frame
-- is the canvas's element id. polls is small, so the check and the index
-- build here.
ALTER TABLE polls
    ADD COLUMN IF NOT EXISTS board_uuid uuid,
    ADD COLUMN IF NOT EXISTS frame_id varchar(128);

ALTER TABLE polls DROP CONSTRAINT IF EXISTS polls_board_frame_check;
ALTER TABLE polls ADD CONSTRAINT polls_board_frame_check CHECK (
    (board_uuid IS NULL AND frame_id IS NULL)
    OR (board_uuid IS NOT NULL AND frame_id IS NOT NULL AND char_length(frame_id) BETWEEN 1 AND 128
        AND channel_id = '00000000-0000-0000-0000-000000000000' AND session_id IS NULL));

-- A board's polls, oldest first.
CREATE INDEX IF NOT EXISTS polls_board_idx ON polls (board_uuid, created_at) WHERE board_uuid IS NOT NULL;
