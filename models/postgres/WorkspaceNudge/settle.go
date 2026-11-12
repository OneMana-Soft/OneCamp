package models

// Open nudges settled by the engine's pass (V-581, D2): superseded when what
// they point at no longer needs its person, quiet when not raised again for a
// while. A status or priority change, never a delete.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ListOpenOfKind is every person's open nudges of kind, oldest first, at
// most limit.
func ListOpenOfKind(ctx context.Context, kind string, limit int) ([]*Nudge, error) {
	return pgexec.Query(ctx, `
		SELECT id, user_id, kind, title, body, cta_url, cta_text,
		       source_type, source_id, status, priority, created_at, updated_at
		FROM workspace_nudges
		WHERE kind = $1 AND status = 'open'
		ORDER BY created_at
		LIMIT $2`, func(r pgexec.Row) (*Nudge, error) { return scanNudge(r) }, kind, limit)
}

// SupersedeByIDs supersedes those of these nudges still open, and answers
// the ones it closed, by whose they were.
func SupersedeByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	type closed struct{ id, user uuid.UUID }
	rows, err := pgexec.Query(ctx, `
		UPDATE workspace_nudges SET status = 'superseded', updated_at = NOW()
		WHERE id = ANY($1) AND status = 'open'
		RETURNING id, user_id`, func(r pgexec.Row) (c closed, err error) { err = r.Scan(&c.id, &c.user); return }, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	byUser := map[uuid.UUID][]uuid.UUID{}
	for _, c := range rows {
		byUser[c.user] = append(byUser[c.user], c.id)
	}
	return byUser, nil
}

// QuietUnraisedSince drops every open urgent nudge last raised before cutoff
// to priority 0: still listed, no longer urgent. When it was raised
// (updated_at) is left as it was. It answers whose nudges went quiet.
func QuietUnraisedSince(ctx context.Context, cutoff time.Time) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `
		WITH quiet AS (
			UPDATE workspace_nudges SET priority = 0
			WHERE status = 'open' AND priority > 0 AND updated_at < $1
			RETURNING user_id
		)
		SELECT DISTINCT user_id FROM quiet`, func(r pgexec.Row) (u uuid.UUID, err error) { err = r.Scan(&u); return }, cutoff)
}
