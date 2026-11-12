-- A board's calls (miro-27, calls with the board): the calls that showed a
-- board, read from the board's side, the one that showed it latest first.
-- call_session_boards' key starts with the call, so reading one board's calls
-- went through the whole table. The session ends the key so that the index
-- holds the read's whole order: a page starts by its key (CallsOfBoard) and
-- reads only its own rows, with nothing to sort.
CREATE INDEX IF NOT EXISTS call_session_boards_board_idx
    ON call_session_boards (board_uuid, first_shown_at DESC, session_id);
