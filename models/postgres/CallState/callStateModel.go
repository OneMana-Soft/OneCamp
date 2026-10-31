package models

// Postgres access for Call State (migrations/*_call_sessions.up.sql): every
// call, who was in it, and the recordings made during it. business/CallState
// owns the rules and business/CallControl what hosts may do; this file only
// reads and writes rows. Ids are made by the caller.
//
// Times come from the caller (LiveKit's webhook says when each join and leave
// happened), so a webhook that arrives late, twice or out of order changes
// nothing it shouldn't: a leave older than the latest join is ignored, and so
// is a join older than the latest leave.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// What a call is, as the kind column holds it: a huddle in a conversation, or
// a meeting with a link of its own.
const (
	KindHuddle  = "huddle"
	KindMeeting = "meeting"
	// KindBreakout is a breakout room of a channel's call or a meeting
	// (calls package C): a call of its own, its parent's (parent_session_id).
	KindBreakout = "breakout"
)

// What a call belongs to (resource_kind): business/Conversation's kinds, or a
// meeting.
const (
	ResourceChannel = "channel"
	ResourceDM      = "dm"
	ResourceGroup   = "group"
	ResourceMeeting = "meeting"
)

// Someone's part in one call.
const (
	RoleHost        = "host"
	RoleCohost      = "cohost"
	RoleParticipant = "participant"
	RoleGuest       = "guest"
	// RoleInvitee is a member in a conversation's call through an invite
	// (HU13), who isn't in the conversation: they run nothing, even in a DM
	// or a group, and the call is never handed to them.
	RoleInvitee = "invitee"
)

// What the people running a call can hold for everyone else (calls meet-12,
// zoom-14), as the held and allowed columns keep it.
const (
	HoldShare      = "share"
	HoldChat       = "chat"
	HoldReact      = "react"
	HoldMicrophone = "microphone"
	HoldCamera     = "camera"
)

// Holds is every hold, in the order a host's switches show them.
var Holds = []string{HoldShare, HoldChat, HoldReact, HoldMicrophone, HoldCamera}

// RunsCall is whether a part runs the call: the call's holds never hold them. Pure.
func RunsCall(role string) bool { return role == RoleHost || role == RoleCohost }

// Why a call ended.
const (
	EndedLastLeft     = "last_left"
	EndedRoomFinished = "room_finished"
	EndedReconciled   = "reconciled"
	EndedByHost       = "host_ended"
	// EndedRoomsClosed ends a breakout room's call: its rooms closed, or its
	// parent call ended.
	EndedRoomsClosed = "rooms_closed"
)

// Session is one call_sessions row.
type Session struct {
	ID           uuid.UUID
	Room         string
	RoomSID      string
	Kind         string
	ResourceKind string
	ResourceID   string
	PostUUID     *uuid.UUID
	Topic        string
	StartedBy    *uuid.UUID
	StartedAt    time.Time
	EndedAt      *time.Time
	EndedReason  string
	LockedAt     *time.Time
	LockedBy     string
	Spotlight    string
	MuteOnEntry  bool
	// Held is what everyone who doesn't run the call may not do now (Holds).
	Held []string
	// The call's Q&A (calls package A): whether people may ask, which
	// questions the people who don't run the call see ("all" or "answered",
	// business/CallQA), and whether they may ask anonymously.
	QAOpen      bool
	QAShown     string
	QAAnonymous bool
	// The board on the call's stage (calls package B, boardModel.go): which,
	// who put it there, who draws on it, and whether its guests do.
	BoardUUID       *uuid.UUID
	BoardBy         string
	BoardDraw       string
	BoardGuestsDraw bool
	// A breakout room's call is its parent's (calls package C,
	// breakoutModel.go); a parent's rooms are open since BreakoutsOpenAt,
	// people choose their own while BreakoutsChoose, they close by themselves
	// at BreakoutsEndsAt, and once closed everyone is called back at
	// BreakoutsClosingAt.
	ParentSessionID    *uuid.UUID
	BreakoutsOpenAt    *time.Time
	BreakoutsChoose    bool
	BreakoutsEndsAt    *time.Time
	BreakoutsClosingAt *time.Time
}

// Open is whether the call is still going on.
func (s Session) Open() bool { return s.EndedAt == nil }

// Attendee is one call_attendance row.
type Attendee struct {
	SessionID    uuid.UUID
	Identity     string
	UserUUID     *uuid.UUID
	Name         string
	Role         string
	JoinedAt     time.Time
	LastJoinedAt time.Time
	LeftAt       *time.Time
	SecondsIn    int
	RemovedBy    string
	// HandRaisedAt is when their hand went up, nil while it is down: the
	// queue's order (migration *_call_hands).
	HandRaisedAt *time.Time
	// Feedback is what stays beside their name until removed (FeedbackKinds), or "".
	Feedback string
	// Allowed is what they may do although the call holds it, as a host let them (Holds).
	Allowed []string
}

// In is whether they are in the call now.
func (a Attendee) In() bool { return a.LeftAt == nil }

// Recording is one call_recordings row.
type Recording struct {
	EgressID  string
	SessionID uuid.UUID
	StartedBy *uuid.UUID
	StartedAt time.Time
	EndedAt   *time.Time
	Status    string
}

// ErrNoDatabase is a server with no database connected (a tool, a unit
// test): every read and write answers it instead of panicking, and each
// caller decides what a call does without its record.
var ErrNoDatabase = errors.New("callstate: no database connected")

// defaultTimeout bounds a query when the connection names no timeout.
const defaultTimeout = 5 * time.Second

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	d := defaultTimeout
	if postgresInit.DBConn != nil && postgresInit.DBConn.DBTimeout > 0 {
		d = postgresInit.DBConn.DBTimeout
	}
	return context.WithTimeout(ctx, d)
}

func db() (*sql.DB, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, ErrNoDatabase
	}
	return postgresInit.DBConn.SqlDB, nil
}

const sessionColumns = `id, room, COALESCE(room_sid, ''), kind, resource_kind, resource_id, post_uuid,
	COALESCE(topic, ''), started_by, started_at, ended_at, COALESCE(ended_reason, ''), locked_at,
	COALESCE(locked_by, ''), COALESCE(spotlight, ''), mute_on_entry, held, qa_open, qa_shown, qa_anonymous,
	board_uuid, COALESCE(board_by, ''), board_draw, board_guests_draw,
	parent_session_id, breakouts_open_at, breakouts_choose, breakouts_ends_at, breakouts_closing_at`

type scanner interface{ Scan(dest ...any) error }

func scanSession(row scanner) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Room, &s.RoomSID, &s.Kind, &s.ResourceKind, &s.ResourceID, &s.PostUUID,
		&s.Topic, &s.StartedBy, &s.StartedAt, &s.EndedAt, &s.EndedReason, &s.LockedAt,
		&s.LockedBy, &s.Spotlight, &s.MuteOnEntry, pq.Array(&s.Held), &s.QAOpen, &s.QAShown, &s.QAAnonymous,
		&s.BoardUUID, &s.BoardBy, &s.BoardDraw, &s.BoardGuestsDraw,
		&s.ParentSessionID, &s.BreakoutsOpenAt, &s.BreakoutsChoose, &s.BreakoutsEndsAt, &s.BreakoutsClosingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func querySession(ctx context.Context, where string, args ...any) (*Session, error) {
	conn, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	return scanSession(conn.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM call_sessions WHERE `+where, args...))
}

// OpenSession makes s the room's call when none is open, and answers the
// room's open call either way, with whether this made it. Two joins at once
// open one call between them.
func OpenSession(ctx context.Context, s Session) (*Session, bool, error) {
	conn, err := db()
	if err != nil {
		return nil, false, err
	}
	qctx, cancel := timeout(ctx)
	var id uuid.UUID
	err = conn.QueryRowContext(qctx, `
		INSERT INTO call_sessions (id, room, room_sid, kind, resource_kind, resource_id, started_by, started_at, parent_session_id)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9)
		ON CONFLICT (room) WHERE ended_at IS NULL DO NOTHING
		RETURNING id`,
		s.ID, s.Room, s.RoomSID, s.Kind, s.ResourceKind, s.ResourceID, s.StartedBy, s.StartedAt, s.ParentSessionID).Scan(&id)
	cancel()
	made := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	open, err := OpenSessionOf(ctx, s.Room)
	if err != nil {
		return nil, false, err
	}
	if open == nil {
		// Ended between the insert and the read: the caller tries again.
		return nil, false, errors.New("callstate: the room's call ended while it was being opened")
	}
	return open, made && open.ID == s.ID, nil
}

// OpenSessionOf answers the call going on in a room, or nil.
func OpenSessionOf(ctx context.Context, room string) (*Session, error) {
	return querySession(ctx, `room = $1 AND ended_at IS NULL`, room)
}

// LatestSessionOf answers a room's latest call, going on or over, or nil.
func LatestSessionOf(ctx context.Context, room string) (*Session, error) {
	return querySession(ctx, `room = $1 ORDER BY started_at DESC LIMIT 1`, room)
}

// SessionByID answers one call, or nil.
func SessionByID(ctx context.Context, id uuid.UUID) (*Session, error) {
	return querySession(ctx, `id = $1`, id)
}

// SessionBySID answers the call LiveKit's room id names, or nil.
func SessionBySID(ctx context.Context, roomSID string) (*Session, error) {
	if roomSID == "" {
		return nil, nil
	}
	return querySession(ctx, `room_sid = $1 ORDER BY started_at DESC LIMIT 1`, roomSID)
}

// CurrentSession answers the call going on in a conversation or meeting, or nil.
func CurrentSession(ctx context.Context, resourceKind, resourceID string) (*Session, error) {
	return querySession(ctx, `resource_kind = $1 AND resource_id = $2 AND ended_at IS NULL AND kind <> 'breakout' ORDER BY started_at DESC LIMIT 1`, resourceKind, resourceID)
}

// OpenSessions answers every call still going on, oldest first, at most limit.
func OpenSessions(ctx context.Context, limit int) ([]Session, error) {
	conn, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT `+sessionColumns+` FROM call_sessions WHERE ended_at IS NULL ORDER BY started_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// EndSession ends a call that is still going on, and closes the attendance of
// everyone still in it, answering whether this ended it (a second end, from a
// repeated webhook or a sweep, changes nothing).
func EndSession(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	conn, err := db()
	if err != nil {
		return false, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE call_sessions SET ended_at = $3, ended_reason = $2 WHERE id = $1 AND ended_at IS NULL`, id, reason, at)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE call_attendance
		SET left_at = GREATEST($2::timestamptz, last_joined_at),
		    seconds_in = seconds_in + GREATEST(0, EXTRACT(EPOCH FROM ($2::timestamptz - last_joined_at)))::int
		WHERE session_id = $1 AND left_at IS NULL`, id, at); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Joined records that someone joined a call at a time, answering whether it
// was their first join and whether it was a return after leaving. A join that
// is older than their latest leave, or that repeats one already recorded,
// answers false for both and changes nothing. role is kept from the first
// join: a co-host who drops and comes back is still a co-host.
func Joined(ctx context.Context, a Attendee, at time.Time) (first, back bool, err error) {
	conn, err := db()
	if err != nil {
		return false, false, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	var inserted bool
	err = conn.QueryRowContext(ctx, `
		INSERT INTO call_attendance (session_id, identity, user_uuid, name, role, joined_at, last_joined_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (session_id, identity) DO UPDATE
			SET last_joined_at = EXCLUDED.last_joined_at, left_at = NULL, name = EXCLUDED.name
			WHERE call_attendance.left_at IS NOT NULL AND call_attendance.left_at < EXCLUDED.last_joined_at
		RETURNING (xmax = 0)`,
		a.SessionID, a.Identity, a.UserUUID, a.Name, a.Role, at).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return inserted, !inserted, nil
}

// Left records that someone left a call at a time, answering whether it
// changed anything. A leave older than their latest join is ignored. A leave
// whose join was never recorded (the join's webhook lost or still on its way)
// is kept as a moment in the call, so a later copy of that join can't put them
// back in.
func Left(ctx context.Context, a Attendee, at time.Time) (bool, error) {
	conn, err := db()
	if err != nil {
		return false, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	res, err := conn.ExecContext(ctx, `
		UPDATE call_attendance
		SET left_at = $3,
		    seconds_in = seconds_in + GREATEST(0, EXTRACT(EPOCH FROM ($3::timestamptz - last_joined_at)))::int
		WHERE session_id = $1 AND identity = $2 AND left_at IS NULL AND last_joined_at <= $3`,
		a.SessionID, a.Identity, at)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return n > 0, err
	}
	res, err = conn.ExecContext(ctx, `
		INSERT INTO call_attendance (session_id, identity, user_uuid, name, role, joined_at, last_joined_at, left_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $6)
		ON CONFLICT (session_id, identity) DO NOTHING`,
		a.SessionID, a.Identity, a.UserUUID, a.Name, a.Role, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const attendeeColumns = `session_id, identity, user_uuid, name, role, joined_at, last_joined_at, left_at, seconds_in, COALESCE(removed_by, ''), hand_raised_at, COALESCE(feedback, ''), allowed`

func scanAttendee(row scanner) (*Attendee, error) {
	var a Attendee
	err := row.Scan(&a.SessionID, &a.Identity, &a.UserUUID, &a.Name, &a.Role, &a.JoinedAt, &a.LastJoinedAt, &a.LeftAt, &a.SecondsIn, &a.RemovedBy, &a.HandRaisedAt, &a.Feedback, pq.Array(&a.Allowed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Attendees answers everyone who was in a call, in the order they first joined.
func Attendees(ctx context.Context, session uuid.UUID) ([]Attendee, error) {
	conn, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT `+attendeeColumns+` FROM call_attendance WHERE session_id = $1 ORDER BY joined_at, identity`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attendee
	for rows.Next() {
		a, err := scanAttendee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AttendeeOf answers one person's row in a call, or nil.
func AttendeeOf(ctx context.Context, session uuid.UUID, identity string) (*Attendee, error) {
	conn, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	return scanAttendee(conn.QueryRowContext(ctx, `SELECT `+attendeeColumns+` FROM call_attendance WHERE session_id = $1 AND identity = $2`, session, identity))
}

// SetRole changes someone's part in a call, answering whether they were in it.
func SetRole(ctx context.Context, session uuid.UUID, identity, role string) (bool, error) {
	conn, err := db()
	if err != nil {
		return false, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	res, err := conn.ExecContext(ctx, `UPDATE call_attendance SET role = $3 WHERE session_id = $1 AND identity = $2`, session, identity, role)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FeedbackKinds are the nonverbal feedback a person can leave beside their
// name, as the table's check allows.
var FeedbackKinds = []string{"yes", "no", "slow_down", "away"}

// SetHand raises (at) or lowers (nil) someone's hand in a call, answering
// whether it changed. Raising a raised hand keeps its place in the queue.
func SetHand(ctx context.Context, session uuid.UUID, identity string, at *time.Time) (bool, error) {
	q := `UPDATE call_attendance SET hand_raised_at = NULL WHERE session_id = $1 AND identity = $2 AND hand_raised_at IS NOT NULL`
	args := []any{session, identity}
	if at != nil {
		q = `UPDATE call_attendance SET hand_raised_at = $3 WHERE session_id = $1 AND identity = $2 AND hand_raised_at IS NULL`
		args = append(args, *at)
	}
	n, err := pgexec.Exec(ctx, q, args...)
	return n > 0, err
}

// LowerAllHands lowers every raised hand in a call, answering whose.
func LowerAllHands(ctx context.Context, session uuid.UUID) ([]string, error) {
	return pgexec.Query(ctx, `UPDATE call_attendance SET hand_raised_at = NULL WHERE session_id = $1 AND hand_raised_at IS NOT NULL RETURNING identity`,
		func(r pgexec.Row) (string, error) {
			var id string
			err := r.Scan(&id)
			return id, err
		}, session)
}

// SetFeedback sets ("" clears) someone's feedback in a call, answering
// whether it changed.
func SetFeedback(ctx context.Context, session uuid.UUID, identity, feedback string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_attendance SET feedback = NULLIF($3, '') WHERE session_id = $1 AND identity = $2 AND COALESCE(feedback, '') <> $3`, session, identity, feedback)
	return n > 0, err
}

// MarkRemoved records that a host put someone out of a call, so they stay out
// of it until it ends. Kept even when their join was never recorded.
func MarkRemoved(ctx context.Context, a Attendee, by string, at time.Time) error {
	_, err := pgexec.Exec(ctx, `
		INSERT INTO call_attendance (session_id, identity, user_uuid, name, role, joined_at, last_joined_at, removed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
		ON CONFLICT (session_id, identity) DO UPDATE SET removed_by = EXCLUDED.removed_by`,
		a.SessionID, a.Identity, a.UserUUID, a.Name, a.Role, at, by)
	return err
}

// SetLock locks a call (by is who did it) or, with a nil at, unlocks it.
func SetLock(ctx context.Context, session uuid.UUID, by string, at *time.Time) error {
	conn, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	if at == nil {
		by = ""
	}
	_, err = conn.ExecContext(ctx, `UPDATE call_sessions SET locked_at = $2, locked_by = NULLIF($3, '') WHERE id = $1 AND ended_at IS NULL`, session, at, by)
	return err
}

// AttachPost writes the call's post to its session, once: only where none is
// written yet (calls CALLS-3b). Answers whether this write made it.
func AttachPost(ctx context.Context, session, post uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions SET post_uuid = $2 WHERE id = $1 AND post_uuid IS NULL`, session, post)
	return n == 1, err
}

// SetSpotlight puts one person in everyone's view, or with "" nobody.
func SetSpotlight(ctx context.Context, session uuid.UUID, identity string) error {
	conn, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	_, err = conn.ExecContext(ctx, `UPDATE call_sessions SET spotlight = NULLIF($2, '') WHERE id = $1 AND ended_at IS NULL`, session, identity)
	return err
}

// SetHeld holds what (one of Holds) for everyone in a live call who doesn't
// run it, or with on false lets them again. It answers what the call holds
// now, and false when the call isn't live.
func SetHeld(ctx context.Context, session uuid.UUID, what string, on bool) ([]string, bool, error) {
	return pgexec.QueryOne(ctx, `UPDATE call_sessions
		SET held = CASE WHEN $3 THEN array_append(array_remove(held, $2), $2) ELSE array_remove(held, $2) END
		WHERE id = $1 AND ended_at IS NULL RETURNING held`, scanHolds, session, what, on)
}

// SetAllowed lets someone in a live call do what (one of Holds) although the
// call holds it, or with on false takes that back. It answers what they're
// allowed now, and false when the live call has no row for them.
func SetAllowed(ctx context.Context, session uuid.UUID, identity, what string, on bool) ([]string, bool, error) {
	return pgexec.QueryOne(ctx, `UPDATE call_attendance a
		SET allowed = CASE WHEN $4 THEN array_append(array_remove(a.allowed, $3), $3) ELSE array_remove(a.allowed, $3) END
		FROM call_sessions s
		WHERE a.session_id = $1 AND a.identity = $2 AND s.id = a.session_id AND s.ended_at IS NULL
		RETURNING a.allowed`, scanHolds, session, identity, what, on)
}

// ChatHeldOver answers which of posts are the posts of live calls that hold
// chat for someone: a member by their uuid, or a guest by their call identity
// (nil and "" for the one they aren't, which match no one's row). Nobody
// running the call is held, nor anyone it let through. One read, over the
// live posts' index (CALLS-3b).
func ChatHeldOver(ctx context.Context, posts []uuid.UUID, member *uuid.UUID, identity string) (map[uuid.UUID]bool, error) {
	if len(posts) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	return pgexec.Pairs[uuid.UUID, bool](ctx, `SELECT s.post_uuid, true FROM call_sessions s
		WHERE s.post_uuid = ANY($1::uuid[]) AND s.ended_at IS NULL AND $4 = ANY(s.held)
		AND NOT EXISTS (SELECT 1 FROM call_attendance a WHERE a.session_id = s.id
			AND (a.user_uuid = $2 OR a.identity = NULLIF($3, ''))
			AND (a.role IN ($5, $6) OR $4 = ANY(a.allowed)))`,
		pq.Array(posts), member, identity, HoldChat, RoleHost, RoleCohost)
}

func scanHolds(r pgexec.Row) ([]string, error) {
	var holds []string
	err := r.Scan(pq.Array(&holds))
	return holds, err
}

// SetMuteOnEntry turns on or off "people join muted" for a call.
func SetMuteOnEntry(ctx context.Context, session uuid.UUID, on bool) error {
	conn, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	_, err = conn.ExecContext(ctx, `UPDATE call_sessions SET mute_on_entry = $2 WHERE id = $1 AND ended_at IS NULL`, session, on)
	return err
}

// SetQA sets a call's Q&A, and says whether the call was still going on.
func SetQA(ctx context.Context, session uuid.UUID, open bool, shown string, anonymous bool) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_sessions SET qa_open = $2, qa_shown = $3, qa_anonymous = $4 WHERE id = $1 AND ended_at IS NULL`,
		session, open, shown, anonymous)
	return n > 0, err
}

// ClaimReport claims writing an ended call's report of its polls and
// questions: once it's written (ReportDone) nobody may, and a claim older
// than lease is taken as its writer's death and may be claimed again.
func ClaimReport(ctx context.Context, session uuid.UUID, lease time.Duration) (bool, error) {
	_, claimed, err := pgexec.QueryOne(ctx, `
		UPDATE call_sessions SET report_claimed_at = now()
		WHERE id = $1 AND ended_at IS NOT NULL AND report_done_at IS NULL
		  AND (report_claimed_at IS NULL OR report_claimed_at < now() - make_interval(secs => $2))
		RETURNING id`, scanID, session, lease.Seconds())
	return claimed, err
}

// ReportDone marks an ended call's report written, or not needed.
func ReportDone(ctx context.Context, session uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE call_sessions SET report_done_at = now() WHERE id = $1`, session)
	return err
}

// ReportsDue lists calls that ended between since and before with a post and
// no report written or being written: the sweep's (business/Huddle).
func ReportsDue(ctx context.Context, since, before time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `
		SELECT id FROM call_sessions
		WHERE ended_at > $1 AND ended_at < $2 AND post_uuid IS NOT NULL AND report_done_at IS NULL
		  AND (report_claimed_at IS NULL OR report_claimed_at < now() - make_interval(secs => $3))
		ORDER BY ended_at LIMIT $4`, scanID, since, before, lease.Seconds(), limit)
}

// RecordingStarted files a recording under the call going on in its room. A
// room with no open call (a recording started before anyone's join was
// recorded) files nothing, and says so.
func RecordingStarted(ctx context.Context, room, egressID string, by *uuid.UUID, at time.Time) (bool, error) {
	conn, err := db()
	if err != nil {
		return false, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	res, err := conn.ExecContext(ctx, `
		INSERT INTO call_recordings (egress_id, session_id, started_by, started_at)
		SELECT $2, id, $3, $4 FROM call_sessions WHERE room = $1 AND ended_at IS NULL
		ON CONFLICT (egress_id) DO NOTHING`, room, egressID, by, at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RecordingEnded records how a recording ended. One that was never filed (its
// start came before the call's) changes nothing.
func RecordingEnded(ctx context.Context, egressID, status string, at time.Time) error {
	conn, err := db()
	if err != nil {
		return err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	_, err = conn.ExecContext(ctx, `UPDATE call_recordings SET ended_at = $3, status = $2 WHERE egress_id = $1`, egressID, status, at)
	return err
}

// Recordings answers the recordings made during a call, oldest first.
func Recordings(ctx context.Context, session uuid.UUID) ([]Recording, error) {
	conn, err := db()
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT egress_id, session_id, started_by, started_at, ended_at, status FROM call_recordings WHERE session_id = $1 ORDER BY started_at`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.EgressID, &r.SessionID, &r.StartedBy, &r.StartedAt, &r.EndedAt, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeepRecording says who keeps the call going on in a room recorded: the
// person who started its recording, or, with by nil, nobody, once a person
// stops it. A recording ends with its room, and LiveKit closes a call's own
// room while everyone is in its breakout rooms; the call records again as
// them when that room is made again (business/CallControl's resumeRecording),
// and never after a person's Stop.
func KeepRecording(ctx context.Context, room string, by *uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE call_sessions SET recording_by = $2 WHERE room = $1 AND ended_at IS NULL`, room, by)
	return err
}

// RecordingWanted answers who keeps a live call recorded (KeepRecording), nil
// for nobody, and whether a recording of it runs as far as the call's record
// knows: one whose end hasn't reached us yet.
func RecordingWanted(ctx context.Context, session uuid.UUID) (*uuid.UUID, bool, error) {
	type wanted struct {
		by      *uuid.UUID
		running bool
	}
	w, _, err := pgexec.QueryOne(ctx, `
		SELECT s.recording_by, EXISTS (SELECT 1 FROM call_recordings r WHERE r.session_id = s.id AND r.ended_at IS NULL)
		FROM call_sessions s WHERE s.id = $1 AND s.ended_at IS NULL`, func(r pgexec.Row) (wanted, error) {
		var w wanted
		err := r.Scan(&w.by, &w.running)
		return w, err
	}, session)
	return w.by, w.running, err
}
