-- The board on a call's stage (calls package B: zoom-new-whiteboard, and the
-- whiteboard half of meet-10 and zoom-02).
--   call_sessions.board_uuid         the board on the stage now, or NULL.
--   call_sessions.board_by           who put it there: their call identity.
--   call_sessions.board_draw         who in the call draws on it: everyone
--                                    (the default) or only the presenter.
--   call_sessions.board_guests_draw  whether the call's guests draw too: the
--                                    presenter's "Guests can draw", off until
--                                    they turn it on.
--   call_session_boards              every board a call showed, for its post
--                                    once the call is over.
-- A call gives no one anything on a board once the board leaves its stage or
-- the call ends (business/CallState BoardGrant reads live calls only).
-- board_snapshots gains the reason 'guests': the version kept as a call's
-- guests start drawing, which "Undo guests' drawing" goes back to.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_uuid uuid;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_by text;
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_draw text NOT NULL DEFAULT 'everyone';
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_guests_draw boolean NOT NULL DEFAULT false;
-- Whether the board's version from before guests drew was kept in its time on
-- this stage: one per time on the stage, which "Undo guests' drawing" goes
-- back to however often the switch is turned.
ALTER TABLE call_sessions ADD COLUMN IF NOT EXISTS board_guests_kept boolean NOT NULL DEFAULT false;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_sessions_board_draw_kind') THEN
        ALTER TABLE call_sessions ADD CONSTRAINT call_sessions_board_draw_kind
            CHECK (board_draw IN ('everyone', 'presenter'));
    END IF;
END $$;

-- A board's live calls, read at every connect to its canvas.
CREATE INDEX IF NOT EXISTS call_sessions_live_board_idx
    ON call_sessions (board_uuid) WHERE board_uuid IS NOT NULL AND ended_at IS NULL;

CREATE TABLE IF NOT EXISTS call_session_boards (
    "session_id"     uuid        NOT NULL REFERENCES call_sessions (id) ON DELETE CASCADE,
    "board_uuid"     uuid        NOT NULL,
    "shown_by"       text        NOT NULL,
    "first_shown_at" timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, board_uuid)
);

ALTER TABLE board_snapshots DROP CONSTRAINT IF EXISTS board_snapshots_reason_check;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_snapshots_reason_kind') THEN
        ALTER TABLE board_snapshots ADD CONSTRAINT board_snapshots_reason_kind
            CHECK (reason IN ('interval', 'mass_delete', 'manual', 'guests'));
    END IF;
END $$;
