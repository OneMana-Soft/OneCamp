package models

// Meetings' homes and their lobby (migration *_call_lobby): call_meetings and
// call_knocks. business/CallLobby writes both; business/CallRoom writes a
// member's personal room, a meeting of kind personal (migration *_call_rooms
// keeps it to one open room per person).

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Who waits in a meeting's lobby.
const (
	// LobbyGuests: everyone from outside waits to be let in (the default, D-CL-5).
	LobbyGuests = "guests"
	// LobbyNobody: anyone with the link joins at once.
	LobbyNobody = "nobody"
	// LobbyInvitees: an event call's (meet-02). The event's invitees come in
	// at once by their own invitation; everyone else from outside waits.
	LobbyInvitees = "invitees"
)

// What came of a knock.
const (
	KnockAdmitted = "admitted"
	KnockDenied   = "denied"
	KnockGone     = "gone"
)

// Meeting is one call_meetings row.
type Meeting struct {
	ID         string
	Kind       string
	Owner      uuid.UUID
	Title      string
	Lobby      string
	CreatedAt  time.Time
	ArchivedAt *time.Time
}

// Knock is one call_knocks row.
type Knock struct {
	ID        uuid.UUID
	Room      string
	GrantID   uuid.UUID
	Name      string
	Identity  string
	KnockedAt time.Time
	DecidedAt *time.Time
	Decision  string
	DecidedBy string
}

// SaveMeeting records a meeting's home, keeping one already there.
func SaveMeeting(ctx context.Context, m Meeting) error {
	if m.Lobby == "" {
		m.Lobby = LobbyGuests
	}
	if m.Kind == "" {
		m.Kind = "instant"
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO call_meetings (id, kind, owner, title, lobby, created_at) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6) ON CONFLICT (id) DO NOTHING`,
		m.ID, m.Kind, m.Owner, m.Title, m.Lobby, m.CreatedAt)
	return err
}

// MeetingOf answers a meeting's home, or nil.
func MeetingOf(ctx context.Context, id string) (*Meeting, error) {
	m, ok, err := pgexec.QueryOne(ctx, `SELECT id, kind, owner, COALESCE(title, ''), lobby, created_at, archived_at FROM call_meetings WHERE id = $1`,
		func(r pgexec.Row) (Meeting, error) {
			var m Meeting
			err := r.Scan(&m.ID, &m.Kind, &m.Owner, &m.Title, &m.Lobby, &m.CreatedAt, &m.ArchivedAt)
			return m, err
		}, id)
	if err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// KindPersonal is a member's personal room (business/CallRoom).
const KindPersonal = "personal"

// KindEvent is a calendar event's own call (business/CallRoom EventCall).
const KindEvent = "event"

// PersonalMeetingOf answers a member's open personal room, or nil.
func PersonalMeetingOf(ctx context.Context, owner uuid.UUID) (*Meeting, error) {
	m, ok, err := pgexec.QueryOne(ctx, `SELECT id, kind, owner, COALESCE(title, ''), lobby, created_at, archived_at FROM call_meetings
		WHERE owner = $1 AND kind = 'personal' AND archived_at IS NULL`,
		func(r pgexec.Row) (Meeting, error) {
			var m Meeting
			err := r.Scan(&m.ID, &m.Kind, &m.Owner, &m.Title, &m.Lobby, &m.CreatedAt, &m.ArchivedAt)
			return m, err
		}, owner)
	if err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// ArchiveMeeting closes a meeting for good, answering whether it was open.
func ArchiveMeeting(ctx context.Context, id string, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_meetings SET archived_at = $2 WHERE id = $1 AND archived_at IS NULL`, id, at)
	return n > 0, err
}

// SetLobby changes who waits in a meeting's lobby, answering whether the meeting is there.
func SetLobby(ctx context.Context, id, lobby string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_meetings SET lobby = $2 WHERE id = $1`, id, lobby)
	return n > 0, err
}

const knockColumns = `id, room, grant_id, name, COALESCE(identity, ''), knocked_at, decided_at, COALESCE(decision, ''), COALESCE(decided_by, '')`

func scanKnock(r pgexec.Row) (Knock, error) {
	var k Knock
	err := r.Scan(&k.ID, &k.Room, &k.GrantID, &k.Name, &k.Identity, &k.KnockedAt, &k.DecidedAt, &k.Decision, &k.DecidedBy)
	return k, err
}

// AddKnock records someone asking to come in.
func AddKnock(ctx context.Context, k Knock) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO call_knocks (id, room, grant_id, name, knocked_at) VALUES ($1, $2, $3, $4, $5)`, k.ID, k.Room, k.GrantID, k.Name, k.KnockedAt)
	return err
}

// KnockByID answers one knock, or nil.
func KnockByID(ctx context.Context, id uuid.UUID) (*Knock, error) {
	k, ok, err := pgexec.QueryOne(ctx, `SELECT `+knockColumns+` FROM call_knocks WHERE id = $1`, scanKnock, id)
	if err != nil || !ok {
		return nil, err
	}
	return &k, nil
}

// Waiting answers the people waiting in a room's lobby, oldest first.
func Waiting(ctx context.Context, room string) ([]Knock, error) {
	return pgexec.Query(ctx, `SELECT `+knockColumns+` FROM call_knocks WHERE room = $1 AND decided_at IS NULL ORDER BY knocked_at, id`, scanKnock, room)
}

// Decide settles one waiting knock (admitted, denied or gone), answering
// whether it was still waiting. identity is who an admitted guest joins as.
func Decide(ctx context.Context, id uuid.UUID, decision, by, identity string, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE call_knocks SET decision = $2, decided_by = NULLIF($3, ''), identity = NULLIF($4, ''), decided_at = $5 WHERE id = $1 AND decided_at IS NULL`,
		id, decision, by, identity, at)
	return n > 0, err
}

// DecideAll settles every knock still waiting in a room at once (Admit
// all), answering how many it settled.
func DecideAll(ctx context.Context, room, decision, by string, at time.Time) (int64, error) {
	return pgexec.Exec(ctx, `UPDATE call_knocks SET decision = $2, decided_by = NULLIF($3, ''), decided_at = $4 WHERE room = $1 AND decided_at IS NULL`,
		room, decision, by, at)
}

// ExpireKnocks settles as gone every knock still waiting since before, so a
// lobby nobody answered empties: answering which rooms changed, each once.
func ExpireKnocks(ctx context.Context, before time.Time) ([]string, error) {
	rooms, err := pgexec.Query(ctx, `UPDATE call_knocks SET decision = 'gone', decided_at = NOW() WHERE decided_at IS NULL AND knocked_at < $1 RETURNING room`,
		func(r pgexec.Row) (string, error) {
			var room string
			err := r.Scan(&room)
			return room, err
		}, before)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(rooms))
	for _, room := range rooms {
		if !seen[room] {
			seen[room] = true
			out = append(out, room)
		}
	}
	return out, nil
}
