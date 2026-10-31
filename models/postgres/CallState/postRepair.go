package models

// The repair of calls' posts (calls CALLS-3b, business/Huddle): the sink
// writes a call's post once, so a write that failed is found here and tried
// again, until it takes, is refused, or runs out of tries.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// PostlessHuddles is the live conversation calls, started before before, that
// still have no post: never refused, tried fewer than tries times. Oldest
// first, at most limit.
func PostlessHuddles(ctx context.Context, before time.Time, tries, limit int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT id FROM call_sessions
		WHERE ended_at IS NULL AND post_uuid IS NULL AND kind = 'huddle' AND post_refused_at IS NULL
		  AND post_tries < $2 AND started_at < $1
		ORDER BY started_at LIMIT $3`, scanID, before, tries, limit)
}

// scanID reads a row of one session id.
func scanID(r pgexec.Row) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.Scan(&id)
	return id, err
}

// NotePostTry records a failed write of a call's post: one more try, and when
// refused, the refusal, so the repair passes it over from now on.
func NotePostTry(ctx context.Context, session uuid.UUID, refused bool) error {
	_, err := pgexec.Exec(ctx, `UPDATE call_sessions SET post_tries = post_tries + 1,
		post_refused_at = CASE WHEN $2 THEN COALESCE(post_refused_at, now()) ELSE post_refused_at END
		WHERE id = $1`, session, refused)
	return err
}
