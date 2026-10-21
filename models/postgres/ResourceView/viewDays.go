package models

// A doc's or board's views by day (migration 470, docs C6-3): one row per
// resource, day, source and viewer. A viewer's later views the same day add
// to its row at most once in dayThrottle, so reloading counts once. Every
// statement runs through models/pgexec.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// Where a view came from: a member in the app, or a visitor to the published page.
const (
	SourceMember = "member"
	SourcePage   = "page"
)

// dayThrottle is the least time between two of one viewer's views that both count.
const dayThrottle = 30 * time.Minute

// RecordDay counts a viewer's view on day (UTC). viewerKey is a member's uuid
// or a visitor's keyed hash; never an address.
func RecordDay(ctx context.Context, resourceType, resourceUUID, source, viewerKey string, day time.Time) error {
	if resourceUUID == "" || viewerKey == "" {
		return nil
	}
	_, err := pgexec.Exec(ctx,
		`INSERT INTO resource_view_days (resource_type, resource_uuid, day, source, viewer_key, views, last_at)
		 VALUES ($1, $2, $3::date, $4, $5, 1, NOW())
		 ON CONFLICT (resource_type, resource_uuid, day, source, viewer_key)
		 DO UPDATE SET views = resource_view_days.views + 1, last_at = NOW()
		 WHERE resource_view_days.last_at < NOW() - make_interval(mins => $6)`,
		resourceType, resourceUUID, day.UTC().Format("2006-01-02"), source, viewerKey, int(dayThrottle.Minutes()))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ResourceView RecordDay failed: %+v", err)
	}
	return err
}

// DayCount is one day's views and viewers from one source.
type DayCount struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC
	Source  string `json:"source"`
	Views   int    `json:"views"`
	Viewers int    `json:"viewers"`
}

// DaySeries is a resource's views and viewers by day and source since from
// (UTC), oldest first. Days with none are absent.
func DaySeries(ctx context.Context, resourceType, resourceUUID string, from time.Time) ([]DayCount, error) {
	out, err := pgexec.Query(ctx,
		`SELECT to_char(day, 'YYYY-MM-DD'), source, SUM(views)::int, COUNT(*)::int
		 FROM resource_view_days
		 WHERE resource_type = $1 AND resource_uuid = $2 AND day >= $3::date
		 GROUP BY day, source ORDER BY day, source`,
		func(r pgexec.Row) (DayCount, error) {
			var d DayCount
			err := r.Scan(&d.Day, &d.Source, &d.Views, &d.Viewers)
			return d, err
		},
		resourceType, resourceUUID, from.UTC().Format("2006-01-02"))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ResourceView DaySeries failed: %+v", err)
		return nil, err
	}
	if out == nil {
		out = []DayCount{}
	}
	return out, nil
}

// DistinctMembers is how many members viewed a resource since from: members
// are the same key every day, so they add up across days; a page's visitors
// don't (their key changes daily).
func DistinctMembers(ctx context.Context, resourceType, resourceUUID string, from time.Time) (int, error) {
	return pgexec.Scalar[int](ctx,
		`SELECT COUNT(DISTINCT viewer_key)::int FROM resource_view_days
		 WHERE resource_type = $1 AND resource_uuid = $2 AND day >= $3::date AND source = 'member'`,
		resourceType, resourceUUID, from.UTC().Format("2006-01-02"))
}

// PruneDays removes rows of days before before (UTC), at most limit, and
// answers how many it removed.
func PruneDays(ctx context.Context, before time.Time, limit int) (int64, error) {
	n, err := pgexec.Exec(ctx,
		`DELETE FROM resource_view_days WHERE ctid IN (
		   SELECT ctid FROM resource_view_days WHERE day < $1::date LIMIT $2)`,
		before.UTC().Format("2006-01-02"), limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ResourceView PruneDays failed: %+v", err)
	}
	return n, err
}
