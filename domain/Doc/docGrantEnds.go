package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// DocGrantEnds is the docs holding a grant that ends by `by`, each with its
// earliest such end, keyed by doc uuid (lower case), at most limit of them:
// what a doc open must know about grants that end for the next while (H6
// A2-2). One statement, on the access_grant_expiry_due index.
func DocGrantEnds(ctx context.Context, by time.Time, limit int) (map[string]time.Time, error) {
	type end struct {
		doc uuid.UUID
		at  time.Time
	}
	got, err := pgexec.Query(ctx, `SELECT resource_uuid, MIN(expires_at) FROM access_grant_expiry
		WHERE resource_kind = 'doc' AND expires_at <= $1 GROUP BY resource_uuid LIMIT $2`,
		func(r pgexec.Row) (end, error) {
			var e end
			err := r.Scan(&e.doc, &e.at)
			return e, err
		}, by, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Doc DocGrantEnds err: %+v", err)
		return nil, err
	}
	out := make(map[string]time.Time, len(got))
	for _, e := range got {
		out[e.doc.String()] = e.at
	}
	return out, nil
}
