-- Slides control (meet-14, Meet GM-49): the board on a call's stage shown as
-- slides, its frames in the order the board's Frames panel keeps.
--   call_sessions.board_slide  the frame everyone in the call is shown: its
--                              element id on the board, or NULL for the whole
--                              board. It belongs to the board's time on the
--                              stage, so putting a board there or taking it
--                              off clears it (CallState boardModel.go).
-- Call Control checks a slide's shape before it writes one; the constraint
-- holds the same shape here, and keeps a slide off a stage with no board.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_slide text;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_sessions_board_slide_shape') THEN
        ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_board_slide_shape
            CHECK (board_slide IS NULL OR (board_uuid IS NOT NULL AND board_slide ~ '^[A-Za-z0-9_-]{1,64}$'));
    END IF;
END $$;
