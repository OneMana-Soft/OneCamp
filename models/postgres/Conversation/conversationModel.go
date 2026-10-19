package models

// Postgres access for conversations (migrations/*_conversations.up.sql): one row
// for each channel, DM and group, who is in each, and the message ledger.
// business/Conversation owns the rules and domain/Conversation the writes the
// membership functions make; this file only reads and writes rows. Ids are made
// by the caller.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The kinds of conversation, as the kind column holds them.
const (
	KindChannel = "channel"
	KindDM      = "dm"
	KindGroup   = "group"
)

// The roles a member has.
const (
	RoleMember  = "member"
	RoleManager = "manager"
)

// Conversation is one conversations row.
type Conversation struct {
	ID          uuid.UUID
	Kind        string
	Ref         string
	ContinuedIn *uuid.UUID
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
}

// Member is one conversation_members row.
type Member struct {
	ConversationID uuid.UUID
	UserID         uuid.UUID
	Role           string
	JoinedAt       time.Time
	LeftAt         *time.Time
	HiddenAt       *time.Time
	UpdatedAt      time.Time
}

// Live is whether the member is in the conversation now.
func (m Member) Live() bool { return m.LeftAt == nil }

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// execer is a pool or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func db() execer { return postgresInit.DBConn.SqlDB }

// Ensure makes c's row when there is none for its ref, and answers the id the
// conversation has: c.ID for a new row, the existing row's otherwise.
func Ensure(ctx context.Context, c Conversation) (uuid.UUID, error) {
	return ensure(ctx, db(), c)
}

func ensure(ctx context.Context, ex execer, c Conversation) (uuid.UUID, error) {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	var id uuid.UUID
	var err error
	// Twice at most: a row another writer made while this statement ran is
	// outside its snapshot, so the first try can see neither its own insert
	// (there was a conflict) nor the other's row; the second sees it.
	for try := 0; try < 2; try++ {
		cctx, cancel := timeout(ctx)
		err = ex.QueryRowContext(cctx, `
			WITH made AS (
				INSERT INTO conversations (id, kind, ref, created_by, created_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (ref) DO NOTHING
				RETURNING id
			)
			SELECT id FROM made
			UNION ALL
			SELECT id FROM conversations WHERE ref = $3
			LIMIT 1`, c.ID, c.Kind, c.Ref, c.CreatedBy, c.CreatedAt).Scan(&id)
		cancel()
		if !errors.Is(err, sql.ErrNoRows) {
			break
		}
	}
	return id, err
}

// EnsureMany makes the rows of cs that have none (by ref), and answers the id
// each of them has, by ref: the backfill's and the reconcile's write.
func EnsureMany(ctx context.Context, cs []Conversation) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	if len(cs) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(cs))
	kinds := make([]string, len(cs))
	refs := make([]string, len(cs))
	by := make([]string, len(cs))
	at := make([]string, len(cs))
	for i, c := range cs {
		ids[i], kinds[i], refs[i] = c.ID, c.Kind, c.Ref
		if c.CreatedBy != nil {
			by[i] = c.CreatedBy.String()
		}
		t := c.CreatedAt
		if t.IsZero() {
			t = time.Now()
		}
		at[i] = t.UTC().Format(time.RFC3339Nano)
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.BulkTimeout)
	defer cancel()
	if _, err := db().ExecContext(cctx, `
		INSERT INTO conversations (id, kind, ref, created_by, created_at)
		SELECT i, k, r, NULLIF(b, '')::uuid, a::timestamptz
		FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[]) AS c(i, k, r, b, a)
		ON CONFLICT (ref) DO NOTHING`,
		uuidArray(ids), pq.Array(kinds), pq.Array(refs), pq.Array(by), pq.Array(at)); err != nil {
		return nil, err
	}
	got, err := ByRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	for ref, c := range got {
		out[ref] = c.ID
	}
	return out, nil
}

// RefsPage is a page of the index's conversations in the order of their refs,
// after after: what the reconcile reads for the conversations the graph no
// longer has.
func RefsPage(ctx context.Context, after string, limit int) ([]Conversation, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT id, kind, ref, continued_in, created_by, created_at
		FROM conversations WHERE ref > $1 ORDER BY ref LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Kind, &c.Ref, &c.ContinuedIn, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ByRefs is the conversations rows for refs, by ref; a ref with none is left out.
func ByRefs(ctx context.Context, refs []string) (map[string]Conversation, error) {
	out := map[string]Conversation{}
	if len(refs) == 0 {
		return out, nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT id, kind, ref, continued_in, created_by, created_at
		FROM conversations WHERE ref = ANY($1::text[])`, pq.Array(refs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Kind, &c.Ref, &c.ContinuedIn, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		out[c.Ref] = c
	}
	return out, rows.Err()
}

// Join makes each of users a live member of the conversation from at. Someone
// who had left joins again, as role; someone in it already keeps their row,
// and is made a manager when role is one (Join never demotes: SetRole does).
func Join(ctx context.Context, conversation uuid.UUID, users []uuid.UUID, role string, at time.Time) error {
	if len(users) == 0 {
		return nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		INSERT INTO conversation_members (conversation_id, user_id, role, joined_at, updated_at)
		SELECT $1, u, $3, $4, $4 FROM unnest($2::uuid[]) AS u
		ON CONFLICT (conversation_id, user_id) DO UPDATE SET
			role = CASE
				WHEN conversation_members.left_at IS NOT NULL THEN EXCLUDED.role
				WHEN EXCLUDED.role = 'manager' THEN 'manager'
				ELSE conversation_members.role END,
			joined_at = CASE WHEN conversation_members.left_at IS NOT NULL THEN EXCLUDED.joined_at ELSE conversation_members.joined_at END,
			left_at = NULL,
			updated_at = EXCLUDED.updated_at`,
		conversation, uuidArray(users), role, at)
	return err
}

// Leave takes each of users out of the conversation at at: a member's row
// keeps when they left (and goes back to being a member's), and someone with
// no row gets one that says they're out, so a backfill writing what it read
// before can't bring them back.
func Leave(ctx context.Context, conversation uuid.UUID, users []uuid.UUID, at time.Time) error {
	if len(users) == 0 {
		return nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		INSERT INTO conversation_members (conversation_id, user_id, role, joined_at, left_at, updated_at)
		SELECT $1, u, 'member', $3, $3, $3 FROM unnest($2::uuid[]) AS u
		ON CONFLICT (conversation_id, user_id) DO UPDATE SET
			left_at = COALESCE(conversation_members.left_at, EXCLUDED.left_at),
			role = 'member',
			updated_at = EXCLUDED.updated_at`,
		conversation, uuidArray(users), at)
	return err
}

// SetRole sets a live member's role; anyone else is left as they are.
func SetRole(ctx context.Context, conversation, user uuid.UUID, role string, at time.Time) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		UPDATE conversation_members SET role = $3, updated_at = $4
		WHERE conversation_id = $1 AND user_id = $2 AND left_at IS NULL`, conversation, user, role, at)
	return err
}

// InsertIfAbsent writes members that have no row yet, and leaves every row
// there is as it is: the backfill's write, which a join or a removal made
// beside it outranks.
func InsertIfAbsent(ctx context.Context, members []Member) (int64, error) {
	if len(members) == 0 {
		return 0, nil
	}
	conv := make([]uuid.UUID, len(members))
	users := make([]uuid.UUID, len(members))
	roles := make([]string, len(members))
	joined := make([]string, len(members))
	for i, m := range members {
		conv[i], users[i], roles[i] = m.ConversationID, m.UserID, m.Role
		at := m.JoinedAt
		if at.IsZero() {
			at = time.Now()
		}
		joined[i] = at.UTC().Format(time.RFC3339Nano)
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.BulkTimeout)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		INSERT INTO conversation_members (conversation_id, user_id, role, joined_at, updated_at)
		SELECT c, u, r, j, NOW() FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::timestamptz[]) AS m(c, u, r, j)
		ON CONFLICT (conversation_id, user_id) DO NOTHING`,
		uuidArray(conv), uuidArray(users), pq.Array(roles), pq.Array(joined))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RowsOf is every member row (live and left) of the conversations, by
// conversation: what the reconcile compares with the graph.
func RowsOf(ctx context.Context, conversations []uuid.UUID) (map[uuid.UUID][]Member, error) {
	out := map[uuid.UUID][]Member{}
	if len(conversations) == 0 {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.BulkTimeout)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT conversation_id, user_id, role, joined_at, left_at, hidden_at, updated_at
		FROM conversation_members WHERE conversation_id = ANY($1::uuid[])`, uuidArray(conversations))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out[m.ConversationID] = append(out[m.ConversationID], m)
	}
	return out, rows.Err()
}

// MemberOf is one person's row in one conversation, or nil.
func MemberOf(ctx context.Context, conversation, user uuid.UUID) (*Member, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	row := db().QueryRowContext(cctx, `
		SELECT conversation_id, user_id, role, joined_at, left_at, hidden_at, updated_at
		FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`, conversation, user)
	m, err := scanMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanMember(s scanner) (Member, error) {
	var m Member
	err := s.Scan(&m.ConversationID, &m.UserID, &m.Role, &m.JoinedAt, &m.LeftAt, &m.HiddenAt, &m.UpdatedAt)
	return m, err
}

// uuidArray is ids as a Postgres uuid[] parameter.
func uuidArray(ids []uuid.UUID) any {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return pq.Array(s)
}
