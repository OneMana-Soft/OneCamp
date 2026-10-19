package models

// What an access decision reads, for one person and many conversations at once:
// one query per kind (business/Conversation access.go decides from them).

import (
	"context"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ChannelFact is a channel as one person stands in it.
type ChannelFact struct {
	Ref        string
	Private    bool
	PostPolicy string
	Archived   bool
	Member     bool
	Manager    bool
}

// ChannelFacts is each of refs that names a channel, as user stands in it.
func ChannelFacts(ctx context.Context, user uuid.UUID, refs []string) (map[string]ChannelFact, error) {
	out := map[string]ChannelFact{}
	if len(refs) == 0 {
		return out, nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT c.ref, ch.ch_private, ch.post_policy, ch.deleted_at IS NOT NULL,
			m.user_id IS NOT NULL AND m.left_at IS NULL,
			COALESCE(m.left_at IS NULL AND m.role = 'manager', false)
		FROM conversations c
		JOIN channels ch ON ch.id = c.id
		LEFT JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = $1
		WHERE c.kind = 'channel' AND c.ref = ANY($2::text[])`, user, pq.Array(refs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f ChannelFact
		if err := rows.Scan(&f.Ref, &f.Private, &f.PostPolicy, &f.Archived, &f.Member, &f.Manager); err != nil {
			return nil, err
		}
		out[f.Ref] = f
	}
	return out, rows.Err()
}

// GroupFacts is, for each of refs that names a group, whether user is in it.
func GroupFacts(ctx context.Context, user uuid.UUID, refs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(refs) == 0 {
		return out, nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT c.ref, m.user_id IS NOT NULL AND m.left_at IS NULL
		FROM conversations c
		LEFT JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = $1
		WHERE c.kind = 'group' AND c.ref = ANY($2::text[])`, user, pq.Array(refs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var in bool
		if err := rows.Scan(&ref, &in); err != nil {
			return nil, err
		}
		out[ref] = in
	}
	return out, rows.Err()
}

// Person is what a decision needs to know about someone.
type Person struct {
	Active   bool
	External bool
	Bot      bool
}

// People is each of ids that names someone, by id.
func People(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Person, error) {
	out := map[uuid.UUID]Person{}
	if len(ids) == 0 {
		return out, nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT id, deleted_at IS NULL, is_external, is_bot FROM users WHERE id = ANY($1::uuid[])`, uuidArray(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var p Person
		if err := rows.Scan(&id, &p.Active, &p.External, &p.Bot); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}
