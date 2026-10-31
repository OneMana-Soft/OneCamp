package models

// A call's breakout rooms (calls package C, migration *_call_breakouts): the
// rooms a live channel's call or meeting splits into, who is in which, and
// whether they are open. A room's own call is a session of kind breakout
// whose parent_session_id is the call it came from.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BreakoutCountdown is how long everyone has between the rooms closing and
// being called back to the main room, as Meet gives.
const BreakoutCountdown = 30 * time.Second

// BreakoutReturnGrace is how long after everyone was called back the call
// still counts as going on in its rooms: the time their way back to its own
// room takes, which LiveKit may have closed and makes again.
const BreakoutReturnGrace = 2 * time.Minute

// GoesOnInRooms is whether a call's people may be in its breakout rooms
// rather than its own room: while they are open or counting down, and for
// BreakoutReturnGrace after everyone was called back. Its own room may then
// be empty, closed or made again, and none of that ends the call. Pure.
func (s *Session) GoesOnInRooms(now time.Time) bool {
	if s.BreakoutsOpenAt != nil {
		return true
	}
	return s.BreakoutsClosingAt != nil && now.Before(s.BreakoutsClosingAt.Add(BreakoutReturnGrace))
}

// Breakout is one of a call's rooms, numbered from 1.
type Breakout struct {
	Position int
	Name     string
}

// SetBreakouts makes a live call's rooms, named in order, in place of the
// ones it had and every assignment to them, only while its rooms are
// closed. False when the call has ended or its rooms are open (or counting
// down).
func SetBreakouts(ctx context.Context, parent uuid.UUID, names []string) (bool, error) {
	var set bool
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		_, closed, err := pgexec.QueryOne(ctx, `SELECT true FROM call_sessions
			WHERE id = $1 AND ended_at IS NULL AND kind <> 'breakout' AND breakouts_open_at IS NULL
			FOR UPDATE`, scanTrue, parent)
		if err != nil || !closed {
			return err
		}
		if _, err := pgexec.Exec(ctx, `DELETE FROM call_breakouts WHERE parent_session_id = $1`, parent); err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `INSERT INTO call_breakouts (parent_session_id, position, name)
			SELECT $1, n.ord, n.name FROM unnest($2::text[]) WITH ORDINALITY AS n(name, ord)`, parent, pq.Array(names)); err != nil {
			return err
		}
		set = true
		return nil
	})
	return set, err
}

// Breakouts is a call's rooms, in order.
func Breakouts(ctx context.Context, parent uuid.UUID) ([]Breakout, error) {
	return pgexec.Query(ctx, `SELECT position, name FROM call_breakouts WHERE parent_session_id = $1 ORDER BY position`,
		func(r pgexec.Row) (Breakout, error) {
			var b Breakout
			err := r.Scan(&b.Position, &b.Name)
			return b, err
		}, parent)
}

// Assign puts people (their call identities) in a live call's rooms, by
// whom, moving anyone already in one. Only into rooms the call has: false
// when the call has ended or a room isn't one of its.
func Assign(ctx context.Context, parent uuid.UUID, rooms map[string]int, by string) (bool, error) {
	identities := make([]string, 0, len(rooms))
	positions := make([]int64, 0, len(rooms))
	for id, n := range rooms {
		identities = append(identities, id)
		positions = append(positions, int64(n))
	}
	var done bool
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		ok, _, err := pgexec.QueryOne(ctx, `SELECT count(*) = cardinality($2::smallint[]) FROM call_breakouts b
			JOIN call_sessions s ON s.id = b.parent_session_id AND s.ended_at IS NULL
			WHERE b.parent_session_id = $1 AND b.position = ANY($2::smallint[])`,
			scanTrue, parent, pq.Array(uniquePositions(positions)))
		if err != nil || !ok {
			return err
		}
		if _, err := pgexec.Exec(ctx, `INSERT INTO call_breakout_assignments (parent_session_id, identity, position, assigned_by)
			SELECT $1, a.identity, a.position, $4 FROM unnest($2::text[], $3::smallint[]) AS a(identity, position)
			ON CONFLICT (parent_session_id, identity)
			DO UPDATE SET position = EXCLUDED.position, assigned_by = EXCLUDED.assigned_by, assigned_at = now()`,
			parent, pq.Array(identities), pq.Array(positions), by); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// scanTrue reads a row that is there.
func scanTrue(r pgexec.Row) (bool, error) {
	var b bool
	err := r.Scan(&b)
	return b, err
}

// uniquePositions is positions with each room once. Pure.
func uniquePositions(positions []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(positions))
	for _, p := range positions {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// Unassign takes people (their call identities) out of a call's rooms: they
// belong in its main room.
func Unassign(ctx context.Context, parent uuid.UUID, identities []string) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM call_breakout_assignments WHERE parent_session_id = $1 AND identity = ANY($2::text[])`, parent, pq.Array(identities))
	return err
}

// InRooms is everyone in one of a call's open breakout rooms, as Call State
// has them: each call identity, with the LiveKit room they are in.
func InRooms(ctx context.Context, parent uuid.UUID) (map[string]string, error) {
	return pgexec.Pairs[string, string](ctx, `SELECT DISTINCT ON (a.identity) a.identity, s.room
		FROM call_attendance a JOIN call_sessions s ON s.id = a.session_id
		WHERE s.parent_session_id = $1 AND s.ended_at IS NULL AND a.left_at IS NULL
		ORDER BY a.identity, a.joined_at DESC`, parent)
}

// LeftInRooms is which of the calls nobody is in now, in its own room or in
// any of its open breakout rooms, and nobody has been in since before: one
// read for a batch of calls. A call nobody has left counts as left long ago.
func LeftInRooms(ctx context.Context, calls []uuid.UUID, before time.Time) (map[uuid.UUID]bool, error) {
	left := map[uuid.UUID]bool{}
	if len(calls) == 0 {
		return left, nil
	}
	ids, err := pgexec.Query(ctx, `SELECT c.id FROM unnest($1::uuid[]) AS c(id)
		WHERE NOT EXISTS (SELECT 1 FROM call_attendance a JOIN call_sessions s ON s.id = a.session_id
				WHERE (s.id = c.id OR s.parent_session_id = c.id) AND a.left_at IS NULL AND s.ended_at IS NULL)
			AND COALESCE((SELECT max(a.left_at) FROM call_attendance a JOIN call_sessions s ON s.id = a.session_id
				WHERE s.id = c.id OR s.parent_session_id = c.id), 'epoch'::timestamptz) < $2`,
		func(r pgexec.Row) (uuid.UUID, error) {
			var id uuid.UUID
			return id, r.Scan(&id)
		}, pq.Array(calls), before)
	for _, id := range ids {
		left[id] = true
	}
	return left, err
}

// SecondsInRooms is how long each person (by call identity) has been in a
// call's breakout rooms, open or closed: the time of each visit they left,
// and of the one they are in now, up to at.
func SecondsInRooms(ctx context.Context, parent uuid.UUID, at time.Time) (map[string]int, error) {
	all, err := SecondsInRoomsOf(ctx, []uuid.UUID{parent}, at)
	if err != nil {
		return nil, err
	}
	return all[parent], nil
}

// SecondsInRoomsOf is SecondsInRooms for many calls in one read, by call:
// a call with no visits to its rooms has no entry.
func SecondsInRoomsOf(ctx context.Context, parents []uuid.UUID, at time.Time) (map[uuid.UUID]map[string]int, error) {
	out := map[uuid.UUID]map[string]int{}
	if len(parents) == 0 {
		return out, nil
	}
	type row struct {
		parent   uuid.UUID
		identity string
		seconds  int
	}
	rows, err := pgexec.Query(ctx, `SELECT s.parent_session_id, a.identity, SUM(a.seconds_in + CASE WHEN a.left_at IS NULL
			THEN GREATEST(0, EXTRACT(EPOCH FROM ($2::timestamptz - a.last_joined_at)))::int ELSE 0 END)::int
		FROM call_attendance a JOIN call_sessions s ON s.id = a.session_id
		WHERE s.parent_session_id = ANY($1) GROUP BY s.parent_session_id, a.identity`,
		func(r pgexec.Row) (row, error) {
			var x row
			err := r.Scan(&x.parent, &x.identity, &x.seconds)
			return x, err
		}, pq.Array(parents), at)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		if out[x.parent] == nil {
			out[x.parent] = map[string]int{}
		}
		out[x.parent][x.identity] = x.seconds
	}
	return out, nil
}

// Assignments is who is in which of a call's rooms, by call identity.
func Assignments(ctx context.Context, parent uuid.UUID) (map[string]int, error) {
	return pgexec.Pairs[string, int](ctx, `SELECT identity, position FROM call_breakout_assignments WHERE parent_session_id = $1`, parent)
}

// OpenBreakouts opens a live call's rooms, if it has any and they are closed:
// whether people may choose their own, and when they close by themselves
// (nil: when someone running the call closes them).
func OpenBreakouts(ctx context.Context, parent uuid.UUID, choose bool, endsAt *time.Time, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions
		SET breakouts_open_at = $4, breakouts_choose = $2, breakouts_ends_at = $3, breakouts_closing_at = NULL
		WHERE id = $1 AND ended_at IS NULL AND breakouts_open_at IS NULL
		AND EXISTS (SELECT 1 FROM call_breakouts WHERE parent_session_id = $1)`, parent, choose, endsAt, at)
	return n > 0, err
}

// CloseBreakouts starts the countdown that calls everyone in a live call's
// open rooms back, once: it answers when they are called back, and false
// when the rooms weren't open or are already closing.
func CloseBreakouts(ctx context.Context, parent uuid.UUID, at time.Time) (time.Time, bool, error) {
	back := at.Add(BreakoutCountdown)
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions SET breakouts_closing_at = $2
		WHERE id = $1 AND ended_at IS NULL AND breakouts_open_at IS NOT NULL AND breakouts_closing_at IS NULL`, parent, back)
	return back, n > 0, err
}

// FinishBreakouts ends a call's rooms once their countdown has run, for
// whoever claims it first (the call's own timer, or the sweep after a
// restart): true for that one caller alone. breakouts_closing_at stays, as
// when everyone was called back (GoesOnInRooms).
func FinishBreakouts(ctx context.Context, parent uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions
		SET breakouts_open_at = NULL, breakouts_choose = false, breakouts_ends_at = NULL
		WHERE id = $1 AND breakouts_open_at IS NOT NULL AND breakouts_closing_at IS NOT NULL AND breakouts_closing_at <= $2`, parent, at)
	return n > 0, err
}

// DueBreakouts is the live calls whose open rooms should close by themselves
// (their time is up) or whose countdown has run, as of at: the sweep's work
// after a restart.
func DueBreakouts(ctx context.Context, at time.Time, limit int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT id FROM call_sessions
		WHERE ended_at IS NULL AND breakouts_open_at IS NOT NULL
		AND (breakouts_closing_at <= $1 OR (breakouts_closing_at IS NULL AND breakouts_ends_at <= $1))
		ORDER BY id LIMIT $2`,
		func(r pgexec.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := r.Scan(&id)
			return id, err
		}, at, limit)
}

// OpenRoomsOf is the live calls of a call's breakout rooms.
func OpenRoomsOf(ctx context.Context, parent uuid.UUID) ([]Session, error) {
	return pgexec.Query(ctx, `SELECT `+sessionColumns+` FROM call_sessions
		WHERE parent_session_id = $1 AND ended_at IS NULL ORDER BY room`,
		func(r pgexec.Row) (Session, error) {
			s, err := scanSession(r)
			if err != nil || s == nil {
				return Session{}, err
			}
			return *s, nil
		}, parent)
}

// TakeParentsHolds gives a breakout room's call what its parent call holds
// for everyone who doesn't run it, as the room is made: the room's own acts
// (a board on its stage) judge by them.
func TakeParentsHolds(ctx context.Context, room, parent uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE call_sessions r SET held = p.held
		FROM call_sessions p WHERE r.id = $1 AND p.id = $2`, room, parent)
	return err
}

// AdoptRoomSID makes a live call the call of the LiveKit room its own room's
// name now names: LiveKit closed the room while everyone was in the call's
// breakout rooms, and made it again for their way back.
func AdoptRoomSID(ctx context.Context, id uuid.UUID, roomSID string) error {
	_, err := pgexec.Exec(ctx, `UPDATE call_sessions SET room_sid = $2 WHERE id = $1 AND ended_at IS NULL`, id, roomSID)
	return err
}
