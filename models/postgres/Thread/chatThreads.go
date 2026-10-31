package models

// Postgres access for chat threads you follow (migrations/*_chat_threads.up.sql,
// chat P2): when a thread last had a reply, who follows it by their own
// choice, how far each has read, and a person's threads. business/ChatThread
// owns the rules; this file only reads and writes rows, through models/pgexec.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The follow choices (thread_participants.follow).
const (
	FollowAuto = "auto" // follows while taking part
	FollowOn   = "on"   // follows whatever their part
	FollowOff  = "off"  // never follows
)

// following is the rule for who follows a thread, as SQL over p (a
// thread_participants row): on, or auto while taking part beyond reading.
const following = `(p.follow = 'on' OR (p.follow = 'auto' AND p.role IN ('author', 'replier', 'mentioned')))`

// unreadFor is whether the thread t had a reply after p last read it.
const unreadFor = `(t.last_reply_at IS NOT NULL AND t.last_reply_at > COALESCE(p.last_read_at, 'epoch'::timestamptz))`

// participantRanks orders the roles, lowest first: a person keeps the
// highest they have had in a thread (AddParticipant's rule).
var participantRanks = map[string]int{"reader": 0, "mentioned": 1, "replier": 2, "author": 3}

// AddParticipants is AddParticipant for several people in one statement: each
// keeps the higher of their roles and the later of their reads. A person
// named twice in one thread is written once, with the higher role and the
// later read of the two.
func AddParticipants(ctx context.Context, people []Participant) error {
	type key struct{ thread, user uuid.UUID }
	merged := make(map[key]int, len(people))
	var rows []Participant
	for _, p := range people {
		k := key{p.ThreadID, p.UserUUID}
		i, seen := merged[k]
		if !seen {
			merged[k] = len(rows)
			rows = append(rows, p)
			continue
		}
		if participantRanks[p.Role] > participantRanks[rows[i].Role] {
			rows[i].Role = p.Role
		}
		if p.LastReadAt != nil && (rows[i].LastReadAt == nil || p.LastReadAt.After(*rows[i].LastReadAt)) {
			rows[i].LastReadAt = p.LastReadAt
		}
	}
	if len(rows) == 0 {
		return nil
	}
	threads := make([]uuid.UUID, len(rows))
	users := make([]uuid.UUID, len(rows))
	roles := make([]string, len(rows))
	reads := make([]string, len(rows)) // "" for none
	for i, p := range rows {
		threads[i], users[i], roles[i] = p.ThreadID, p.UserUUID, p.Role
		if p.LastReadAt != nil {
			reads[i] = p.LastReadAt.UTC().Format(time.RFC3339Nano)
		}
	}
	_, err := pgexec.Exec(ctx, `
		INSERT INTO thread_participants (thread_id, user_uuid, role, last_read_at)
		SELECT p.thread_id, p.user_uuid, p.role, NULLIF(p.read_at, '')::timestamptz
		FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::text[]) AS p(thread_id, user_uuid, role, read_at)
		ON CONFLICT (thread_id, user_uuid) DO UPDATE SET
		  role = CASE
		    WHEN array_position(ARRAY['reader', 'mentioned', 'replier', 'author'], EXCLUDED.role)
		       > array_position(ARRAY['reader', 'mentioned', 'replier', 'author'], thread_participants.role)
		    THEN EXCLUDED.role ELSE thread_participants.role END,
		  last_read_at = GREATEST(thread_participants.last_read_at, EXCLUDED.last_read_at)`,
		pq.Array(threads), pq.Array(users), pq.Array(roles), pq.Array(reads))
	return err
}

// LastReplyAt is when thread id last had a reply: zero when it has had none,
// or isn't a thread the index keeps. One row by its key: a call's chat asks
// it on every poll (calls CALLS-3b).
func LastReplyAt(ctx context.Context, id uuid.UUID) (time.Time, error) {
	at, _, err := pgexec.QueryOne(ctx, `SELECT last_reply_at FROM threads WHERE id = $1`, func(r pgexec.Row) (time.Time, error) {
		var at sql.NullTime
		err := r.Scan(&at)
		return at.Time, err
	}, id)
	return at, err
}

// TouchReply records a reply in thread id at at: its latest reply, which only
// moves forward. touched is false when there is no such thread.
func TouchReply(ctx context.Context, id uuid.UUID, at time.Time) (touched bool, err error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE threads SET last_reply_at = GREATEST(COALESCE(last_reply_at, $2), $2), updated_at = GREATEST(updated_at, $2)
		WHERE id = $1`, id, at)
	return n == 1, err
}

// SetFollow records user's own choice for thread id (FollowAuto, FollowOn or
// FollowOff), making them one of its people if they weren't: a reader who has
// read it to readAt, so following a thread brings only its later replies as
// new. One of its people keeps how far they had read.
func SetFollow(ctx context.Context, id, user uuid.UUID, follow string, readAt time.Time) error {
	_, err := pgexec.Exec(ctx, `
		INSERT INTO thread_participants (thread_id, user_uuid, role, follow, last_read_at) VALUES ($1, $2, 'reader', $3, $4)
		ON CONFLICT (thread_id, user_uuid) DO UPDATE SET follow = EXCLUDED.follow`, id, user, follow, readAt)
	return err
}

// MarkRead moves user's read of thread id forward to at, when they are one of
// its people, and says whether they are.
func MarkRead(ctx context.Context, id, user uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE thread_participants SET last_read_at = GREATEST(COALESCE(last_read_at, $3), $3)
		WHERE thread_id = $1 AND user_uuid = $2`, id, user, at)
	return n == 1, err
}

// MarkUnread moves user's read of thread id back to just before its latest
// reply, when they are one of its people and it has a reply, and says whether
// it did.
func MarkUnread(ctx context.Context, id, user uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE thread_participants p SET last_read_at = t.last_reply_at - interval '1 microsecond'
		FROM threads t
		WHERE t.id = p.thread_id AND p.thread_id = $1 AND p.user_uuid = $2 AND t.last_reply_at IS NOT NULL`, id, user)
	return n == 1, err
}

// MarkAllRead moves user's read of every thread they follow of kinds forward
// to its latest reply, and says how many were unread.
func MarkAllRead(ctx context.Context, user uuid.UUID, kinds []string) (int, error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE thread_participants p SET last_read_at = t.last_reply_at
		FROM threads t
		WHERE t.id = p.thread_id AND p.user_uuid = $1 AND t.resource_kind = ANY($2) AND `+following+` AND `+unreadFor,
		user, pq.Array(kinds))
	return int(n), err
}

// Followers of thread id: everyone whose choice or part makes them follow it,
// and everyone who chose not to, by user.
func Followers(ctx context.Context, id uuid.UUID) (on []uuid.UUID, off []uuid.UUID, err error) {
	type person struct {
		id             uuid.UUID
		follows, muted bool
	}
	people, err := pgexec.Query(ctx, `
		SELECT p.user_uuid, `+following+` AS follows, p.follow = 'off' AS muted
		FROM thread_participants p WHERE p.thread_id = $1`, func(r pgexec.Row) (p person, err error) {
		err = r.Scan(&p.id, &p.follows, &p.muted)
		return
	}, id)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range people {
		switch {
		case p.muted:
			off = append(off, p.id)
		case p.follows:
			on = append(on, p.id)
		}
	}
	return on, off, nil
}

// Mine is one of user's threads: one they follow, of the kinds asked.
type Mine struct {
	ThreadID     uuid.UUID
	Kind         string
	Conversation string
	LastReplyAt  *time.Time
	LastReadAt   *time.Time
	Follow       string
	Role         string
	Unread       bool
}

// timeOrNil is t as a pointer, nil for SQL NULL. Pure.
func timeOrNil(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// MineOf is the threads user follows of kinds, unread ones first and each
// group newest reply first, limit of them after skipping offset.
func MineOf(ctx context.Context, user uuid.UUID, kinds []string, limit, offset int) ([]Mine, error) {
	return pgexec.Query(ctx, `
		SELECT t.id, t.resource_kind, t.resource_uuid, t.last_reply_at, p.last_read_at, p.follow, p.role, `+unreadFor+` AS unread
		FROM thread_participants p JOIN threads t ON t.id = p.thread_id
		WHERE p.user_uuid = $1 AND t.resource_kind = ANY($2) AND t.last_reply_at IS NOT NULL AND `+following+`
		ORDER BY unread DESC, t.last_reply_at DESC, t.id
		LIMIT $3 OFFSET $4`, func(r pgexec.Row) (m Mine, err error) {
		var reply, read sql.NullTime
		if err = r.Scan(&m.ThreadID, &m.Kind, &m.Conversation, &reply, &read, &m.Follow, &m.Role, &m.Unread); err != nil {
			return m, err
		}
		m.LastReplyAt, m.LastReadAt = timeOrNil(reply), timeOrNil(read)
		return m, nil
	}, user, pq.Array(kinds), limit, offset)
}

// UnreadCount is how many threads user follows of kinds had a reply since
// they last read them.
func UnreadCount(ctx context.Context, user uuid.UUID, kinds []string) (int, error) {
	return pgexec.Scalar[int](ctx, `
		SELECT count(*) FROM thread_participants p JOIN threads t ON t.id = p.thread_id
		WHERE p.user_uuid = $1 AND t.resource_kind = ANY($2) AND `+following+` AND `+unreadFor,
		user, pq.Array(kinds))
}

// ChoiceOf is user's own choice for thread id, and how far they've read it:
// FollowAuto and nil when they are none of its people.
func ChoiceOf(ctx context.Context, id, user uuid.UUID) (follow string, follows bool, readAt *time.Time, err error) {
	type choice struct {
		follow  string
		follows bool
		read    sql.NullTime
	}
	c, found, err := pgexec.QueryOne(ctx, `
		SELECT p.follow, `+following+`, p.last_read_at FROM thread_participants p WHERE p.thread_id = $1 AND p.user_uuid = $2`,
		func(r pgexec.Row) (c choice, err error) {
			err = r.Scan(&c.follow, &c.follows, &c.read)
			return
		}, id, user)
	if err != nil {
		return "", false, nil, err
	}
	if !found {
		return FollowAuto, false, nil, nil
	}
	return c.follow, c.follows, timeOrNil(c.read), nil
}
