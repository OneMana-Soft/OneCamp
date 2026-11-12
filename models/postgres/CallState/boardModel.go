package models

// The board on a call's stage (calls package B, migration *_call_board): one
// at a time, who put it there, who draws on it, and every board a call
// showed.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Who draws on the board on a call's stage (call_sessions.board_draw).
const (
	DrawEveryone  = "everyone"
	DrawPresenter = "presenter"
)

// SetBoard puts a board on a live call's stage for by (their call identity),
// with everyone drawing, the call's guests not, and the whole board shown (no
// slide), and keeps it among the boards the call showed. Unless takeover, it
// goes on only while the stage has no board or by's own. Answers whether it
// went on, and the board it took the stage from (uuid.Nil for none), read
// under the row's lock, so a stop or another present racing it can't make the
// caller close the wrong board's sockets.
func SetBoard(ctx context.Context, session, board uuid.UUID, by string, takeover bool) (bool, uuid.UUID, error) {
	was, on, err := pgexec.QueryOne(ctx, `WITH old AS (
			SELECT id, board_uuid FROM call_sessions WHERE id = $1 FOR UPDATE
		), s AS (
			UPDATE call_sessions c
			SET board_uuid = $2, board_by = $3, board_draw = $5, board_guests_draw = false, board_guests_kept = false,
				board_slide = NULL
			FROM old
			WHERE c.id = old.id AND c.ended_at IS NULL AND ($4 OR c.board_uuid IS NULL OR c.board_by = $3)
			RETURNING old.board_uuid AS was
		), shown AS (
			INSERT INTO call_session_boards (session_id, board_uuid, shown_by)
			SELECT $1, $2, $3 FROM s
			ON CONFLICT (session_id, board_uuid) DO NOTHING
		)
		SELECT was FROM s`, scanNullID, session, board, by, takeover, DrawEveryone)
	return on, was, err
}

// ClearBoard takes the board off a live call's stage, only if by put it there
// unless by is "", answering the board it took off (read under the row's
// lock), uuid.Nil when none was.
func ClearBoard(ctx context.Context, session uuid.UUID, by string) (uuid.UUID, error) {
	was, _, err := pgexec.QueryOne(ctx, `WITH old AS (
			SELECT id, board_uuid FROM call_sessions WHERE id = $1 FOR UPDATE
		)
		UPDATE call_sessions c
		SET board_uuid = NULL, board_by = NULL, board_draw = $2, board_guests_draw = false, board_guests_kept = false,
			board_slide = NULL
		FROM old
		WHERE c.id = old.id AND c.ended_at IS NULL AND c.board_uuid IS NOT NULL AND ($3 = '' OR c.board_by = $3)
		RETURNING old.board_uuid`, scanNullID, session, DrawEveryone, by)
	return was, err
}

// MarkGuestsKept records that the board on a live call's stage had its
// version from before guests drew kept, answering whether this was the first
// time in its time on the stage.
func MarkGuestsKept(ctx context.Context, session uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions SET board_guests_kept = true
		WHERE id = $1 AND ended_at IS NULL AND board_uuid IS NOT NULL AND NOT board_guests_kept`, session)
	return n == 1, err
}

// GuestsKept is whether the board on a call's stage had its version from
// before guests drew kept in its time on the stage.
func GuestsKept(ctx context.Context, session uuid.UUID) (bool, error) {
	kept, _, err := pgexec.QueryOne(ctx, `SELECT board_guests_kept FROM call_sessions WHERE id = $1`,
		func(r pgexec.Row) (bool, error) {
			var b bool
			err := r.Scan(&b)
			return b, err
		}, session)
	return kept, err
}

// BoardState is the board on a call's stage, as its row has it.
type BoardState struct {
	UUID       uuid.UUID
	By         string
	Draw       string
	GuestsDraw bool
	// Slide is the frame everyone is shown, "" for the whole board.
	Slide string
}

// boardStateColumns is a BoardState as a write on call_sessions returns it,
// read by scanBoardState.
const boardStateColumns = `board_uuid, COALESCE(board_by, ''), board_draw, board_guests_draw, COALESCE(board_slide, '')`

func scanBoardState(r pgexec.Row) (BoardState, error) {
	var b BoardState
	err := r.Scan(&b.UUID, &b.By, &b.Draw, &b.GuestsDraw, &b.Slide)
	return b, err
}

// SetBoardDraw sets who draws on the board on a live call's stage: draw
// (DrawEveryone or DrawPresenter) unless nil, and whether the call's guests
// draw too unless nil. Each changes its own column only, so two switches
// flipped at once both hold. Answers the board as it is now, false when none
// was on the stage.
func SetBoardDraw(ctx context.Context, session uuid.UUID, draw *string, guests *bool) (BoardState, bool, error) {
	return pgexec.QueryOne(ctx, `UPDATE call_sessions
		SET board_draw = COALESCE($2, board_draw), board_guests_draw = COALESCE($3, board_guests_draw)
		WHERE id = $1 AND ended_at IS NULL AND board_uuid IS NOT NULL
		RETURNING `+boardStateColumns, scanBoardState, session, draw, guests)
}

// SetBoardSlide shows everyone in a live call slide, a frame on the board on
// its stage ("" for the whole board), only while that board is still there
// and, unless by is "", by put it there: a move racing another board's
// arrival, or a host's takeover of the presenter's stage, lands on nothing.
// Answers the board as it is now, false when the move landed on nothing.
func SetBoardSlide(ctx context.Context, session, board uuid.UUID, by, slide string) (BoardState, bool, error) {
	return pgexec.QueryOne(ctx, `UPDATE call_sessions SET board_slide = NULLIF($4, '')
		WHERE id = $1 AND ended_at IS NULL AND board_uuid = $2 AND ($3 = '' OR board_by = $3)
		RETURNING `+boardStateColumns, scanBoardState, session, board, by, slide)
}

// scanNullID is a nullable uuid column, uuid.Nil for NULL.
func scanNullID(r pgexec.Row) (uuid.UUID, error) {
	var id uuid.NullUUID
	err := r.Scan(&id)
	return id.UUID, err
}

// BoardStage is a live call with a board on its stage, as one person in it
// now sees it.
type BoardStage struct {
	SessionID  uuid.UUID
	Identity   string
	Room       string
	By         string
	Draw       string
	GuestsDraw bool
	Role       string
	Guest      bool
}

// BoardStagesOf answers the live calls with board on their stage that
// identity is in now (by Call State's record; LiveKit has the last word).
func BoardStagesOf(ctx context.Context, board uuid.UUID, identity string) ([]BoardStage, error) {
	return boardStages(ctx, "a.identity = $2", board, identity)
}

// BoardStagesOfMember is BoardStagesOf for a member, by their user uuid.
func BoardStagesOfMember(ctx context.Context, board, user uuid.UUID) ([]BoardStage, error) {
	return boardStages(ctx, "a.user_uuid = $2", board, user)
}

// boardStages answers the live calls with board on their stage, as each one
// in them now whom who ($2) picks sees it.
func boardStages(ctx context.Context, who string, board uuid.UUID, whom any) ([]BoardStage, error) {
	return pgexec.Query(ctx, `SELECT s.id, a.identity, s.room, COALESCE(s.board_by, ''), s.board_draw,
			s.board_guests_draw, a.role, a.user_uuid IS NULL
		FROM call_sessions s JOIN call_attendance a ON a.session_id = s.id
		WHERE s.board_uuid = $1 AND s.ended_at IS NULL AND a.left_at IS NULL AND `+who,
		func(r pgexec.Row) (BoardStage, error) {
			var b BoardStage
			err := r.Scan(&b.SessionID, &b.Identity, &b.Room, &b.By, &b.Draw, &b.GuestsDraw, &b.Role, &b.Guest)
			return b, err
		}, board, whom)
}

// BoardsShown answers the boards a call showed, in the order it first did.
func BoardsShown(ctx context.Context, session uuid.UUID) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT board_uuid FROM call_session_boards WHERE session_id = $1
		ORDER BY first_shown_at, board_uuid`, scanID, session)
}

// BoardCall is a call that showed a board (call_session_boards), as the
// board's calls read it.
type BoardCall struct {
	SessionID uuid.UUID
	Room      string
	RoomSID   string
	StartedAt time.Time
	EndedAt   *time.Time
	// OnStage is whether the board is on the call's stage now.
	OnStage bool
	// ShownBy is the name of whoever first put the board there, as the call
	// knew them, and ShownAt when.
	ShownBy string
	ShownAt time.Time
	// People is how many came to the call.
	People int
}

// BoardCallsAfter is where a page of a board's calls starts: right after the
// call that showed it at ShownAt with the session Session, in CallsOfBoard's
// order. The zero value is the top, the call that showed it latest.
type BoardCallsAfter struct {
	ShownAt time.Time
	Session uuid.UUID
}

// After is the page that starts right after c.
func (c BoardCall) After() BoardCallsAfter {
	return BoardCallsAfter{ShownAt: c.ShownAt, Session: c.SessionID}
}

// boardCallsRead and boardCallsOrder are CallsOfBoard's read, either side of
// where a page starts.
const (
	boardCallsRead = `SELECT s.id, s.room, COALESCE(s.room_sid, ''), s.started_at, s.ended_at,
			s.ended_at IS NULL AND COALESCE(s.board_uuid = b.board_uuid, false),
			COALESCE(a.name, ''), b.first_shown_at,
			(SELECT count(*) FROM call_attendance p WHERE p.session_id = s.id)
		FROM call_session_boards b
		JOIN call_sessions s ON s.id = b.session_id
		LEFT JOIN call_attendance a ON a.session_id = s.id AND a.identity = b.shown_by
		WHERE b.board_uuid = $1`
	boardCallsOrder = `
		ORDER BY b.first_shown_at DESC, b.session_id
		LIMIT $2`
)

// CallsOfBoard answers a page of the calls that showed a board, the one that
// showed it latest first, at most limit, from after (the top when zero). A
// page starts by its key, never by an offset, in the index's own order
// (call_session_boards_board_idx), so a page deep down costs what the first
// one does and a call shown meanwhile neither repeats nor skips one.
func CallsOfBoard(ctx context.Context, board uuid.UUID, after BoardCallsAfter, limit int) ([]BoardCall, error) {
	q, args := boardCallsRead+boardCallsOrder, []any{board, limit}
	if !after.ShownAt.IsZero() {
		// first_shown_at <= $3 starts the index's range; the rest leaves out
		// the calls shown at that same instant up to and including after's.
		q = boardCallsRead + `
		AND b.first_shown_at <= $3 AND (b.first_shown_at < $3 OR b.session_id > $4)` + boardCallsOrder
		args = append(args, after.ShownAt, after.Session)
	}
	return pgexec.Query(ctx, q,
		func(r pgexec.Row) (BoardCall, error) {
			var c BoardCall
			err := r.Scan(&c.SessionID, &c.Room, &c.RoomSID, &c.StartedAt, &c.EndedAt, &c.OnStage, &c.ShownBy, &c.ShownAt, &c.People)
			return c, err
		}, args...)
}
