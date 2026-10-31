package models

// The board on a call's stage (calls package B, migration *_call_board): one
// at a time, who put it there, who draws on it, and every board a call
// showed.

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Who draws on the board on a call's stage (call_sessions.board_draw).
const (
	DrawEveryone  = "everyone"
	DrawPresenter = "presenter"
)

// SetBoard puts a board on a live call's stage for by (their call identity),
// with everyone drawing and the call's guests not, and keeps it among the
// boards the call showed. Unless takeover, it goes on only while the stage has
// no board or by's own. Answers whether it went on, and the board it took the
// stage from (uuid.Nil for none), read under the row's lock, so a stop or
// another present racing it can't make the caller close the wrong board's
// sockets.
func SetBoard(ctx context.Context, session, board uuid.UUID, by string, takeover bool) (bool, uuid.UUID, error) {
	was, on, err := pgexec.QueryOne(ctx, `WITH old AS (
			SELECT id, board_uuid FROM call_sessions WHERE id = $1 FOR UPDATE
		), s AS (
			UPDATE call_sessions c
			SET board_uuid = $2, board_by = $3, board_draw = $5, board_guests_draw = false, board_guests_kept = false
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
		SET board_uuid = NULL, board_by = NULL, board_draw = $2, board_guests_draw = false, board_guests_kept = false
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
		RETURNING board_uuid, COALESCE(board_by, ''), board_draw, board_guests_draw`,
		func(r pgexec.Row) (BoardState, error) {
			var b BoardState
			err := r.Scan(&b.UUID, &b.By, &b.Draw, &b.GuestsDraw)
			return b, err
		}, session, draw, guests)
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
