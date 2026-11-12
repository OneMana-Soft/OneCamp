package models

// Who said no to a DM's or a group's call ringing them (migration
// 576_call_declines, slack-06 H2): business/CallState writes them and
// business/Huddle reads them for the ended call's card. Never deleted.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Decline records that user said no to the call session ringing them, and
// answers whether this is the first time: a repeat keeps the first.
func Decline(ctx context.Context, session, user uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `INSERT INTO call_declines (session_id, user_uuid, declined_at) VALUES ($1, $2, $3)
		ON CONFLICT (session_id, user_uuid) DO NOTHING`, session, user, at)
	return n == 1, err
}

// Declines answers who declined a call, first declined first, at most limit.
func Declines(ctx context.Context, session uuid.UUID, limit int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT user_uuid FROM call_declines WHERE session_id = $1 ORDER BY declined_at, user_uuid LIMIT $2`,
		func(r pgexec.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := r.Scan(&id)
			return id, err
		}, session, limit)
}
