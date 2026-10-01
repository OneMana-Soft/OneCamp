package models

// Postgres access for resource view tracking (migration 87) - the "Viewed by"
// list for docs and boards. One row per (resource, user); the UNIQUE constraint
// makes dedup structural so refreshes never create duplicates. RecordView is
// throttled so a user reloading the page does not amplify writes.

import (
	"context"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Resource types - keep aligned with the CHECK constraint in migration 87.
const (
	ResourceDoc   = "doc"
	ResourceBoard = "board"
)

// recordThrottle is the minimum gap between last_viewed_at bumps for the same
// (resource, user). Refreshes inside this window are no-ops, so a user reloading
// repeatedly cannot hammer the row. First-ever views always insert.
const recordThrottle = 30 * time.Second

// Viewer is one row of the viewer list: a distinct user and when they last
// (and first) viewed the resource.
type Viewer struct {
	UserUUID      string    `json:"user_uuid"`
	FirstViewedAt time.Time `json:"first_viewed_at"`
	LastViewedAt  time.Time `json:"last_viewed_at"`
}

// RecordView upserts a view. On first view it inserts; on a later view it bumps
// last_viewed_at only when the throttle window has elapsed, so refresh storms
// are absorbed without write amplification.
func RecordView(ctx context.Context, resourceType, resourceUUID, userUUID string) error {
	if resourceUUID == "" || userUUID == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Bind the throttle as a string so the `|| ' seconds'` text concatenation
	// type-checks: passing an int here makes pgx try to encode it as text
	// (OID 25) which fails with "cannot find encode plan", silently dropping
	// every view. Postgres casts the resulting "30 seconds" text to interval.
	throttleSeconds := strconv.Itoa(int(recordThrottle.Seconds()))
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`INSERT INTO resource_views (resource_type, resource_uuid, user_uuid, first_viewed_at, last_viewed_at)
		 VALUES ($1, $2, $3, NOW(), NOW())
		 ON CONFLICT (resource_type, resource_uuid, user_uuid)
		 DO UPDATE SET last_viewed_at = NOW()
		 WHERE resource_views.last_viewed_at < NOW() - ($4 || ' seconds')::interval`,
		resourceType, resourceUUID, userUUID, throttleSeconds)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/ResourceView RecordView failed: %+v", err)
	}
	return err
}

// ListViewers returns a resource's distinct viewers, most-recent-first, with
// limit/offset pagination (the list can be as large as the workspace).
func ListViewers(ctx context.Context, resourceType, resourceUUID string, limit, offset int) ([]Viewer, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT user_uuid, first_viewed_at, last_viewed_at
		   FROM resource_views
		  WHERE resource_type = $1 AND resource_uuid = $2
		  ORDER BY last_viewed_at DESC
		  LIMIT $3 OFFSET $4`, resourceType, resourceUUID, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/ResourceView ListViewers failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []Viewer
	for rows.Next() {
		var v Viewer
		if serr := rows.Scan(&v.UserUUID, &v.FirstViewedAt, &v.LastViewedAt); serr != nil {
			return out, serr
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CountViewers returns the number of distinct viewers of a resource.
func CountViewers(ctx context.Context, resourceType, resourceUUID string) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT COUNT(*) FROM resource_views WHERE resource_type = $1 AND resource_uuid = $2`,
		resourceType, resourceUUID).Scan(&n)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/ResourceView CountViewers failed: %+v", err)
		return 0, err
	}
	return n, nil
}
