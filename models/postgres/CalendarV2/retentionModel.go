package models

// The calendar under a retention rule and holds (the governance trail's H2,
// business/Retention): which events a rule may remove, removing them, and a
// held person's events for an admin's search and export.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	retentionModel "github.com/akashc777/OneCamp/models/postgres/Retention"
	"github.com/lib/pq"
)

// lengthOf is an event's length as an interval: a timed event's own, an
// all-day one's days plus one, so an all-day event counts as ended only once
// its last day is over everywhere.
const lengthOf = `COALESCE(e.end_at - e.start_at, (e.end_date - e.start_date + 1) * interval '1 day')`

// removable is the events a rule with cutoff $1 may remove: a single event or
// a whole series (its overrides go with it, by their foreign key), that
//
//   - is on the workspace's own calendars: a subscription's, a holiday
//     calendar's and a connected outside calendar's events stay, because the
//     calendar they come from keeps them;
//   - ended before the cutoff: a series once its last instance has, an added
//     date and an instance moved later included, and a series with no end
//     never;
//   - touches nobody on a hold (retentionModel.HeldPeople, read inside this
//     statement): not its organiser, the owner of its calendar, or a guest of
//     it or of any of its instances, deleted or not.
//
// Deleted events are removed like any other once they ended; a held person's
// are kept like any other.
var removable = `e.series_id IS NULL
	AND e.source <> 'subscribed'
	AND NOT EXISTS (SELECT 1 FROM calendars c WHERE c.id = e.calendar_id AND c.kind IN ('subscribed', 'holidays', 'external'))
	AND (
	  (e.rrule = '' AND ` + endedAt("e") + ` < $1)
	  OR (e.rrule <> '' AND e.until_at IS NOT NULL AND e.until_at + ` + lengthOf + ` < $1
	      AND NOT EXISTS (SELECT 1 FROM unnest(e.rdates) r WHERE r + ` + lengthOf + ` >= $1))
	)
	AND NOT EXISTS (SELECT 1 FROM events o WHERE o.series_id = e.id AND ` + endedAt("o") + ` >= $1)
	AND NOT EXISTS (
	  SELECT 1 FROM events f
	  WHERE (f.id = e.id OR f.series_id = e.id)
	    AND (f.created_by IN (` + retentionModel.HeldPeople + `)
	         OR EXISTS (SELECT 1 FROM calendars fc WHERE fc.id = f.calendar_id AND fc.owner_id IN (` + retentionModel.HeldPeople + `))
	         OR EXISTS (SELECT 1 FROM event_attendees a WHERE a.event_id = f.id AND a.user_id IN (` + retentionModel.HeldPeople + `))))`

// endedAt is when a single event or override (by its table alias) ended,
// an all-day one a day late for the same reason as lengthOf.
func endedAt(alias string) string {
	return `COALESCE(` + alias + `.end_at, (` + alias + `.end_date + 1)::timestamp AT TIME ZONE 'UTC')`
}

// CountRemovable is how many events (a series is one) a rule with cutoff
// would remove now.
func CountRemovable(ctx context.Context, cutoff time.Time) (int64, error) {
	return pgexec.Scalar[int64](ctx, `SELECT count(*) FROM events e WHERE `+removable, cutoff)
}

// PurgeRemovable removes for good at most limit of the events CountRemovable
// counts, with their overrides, guests, answers and reminders (their foreign
// keys cascade), and answers the ids of those it removed, so that what keeps
// a copy of them (the events search index) lets them go too. It is
// retention's one hard delete (helpers' hardDeletes, "purge: "), and it
// re-reads the holds in force as it runs.
func PurgeRemovable(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	return pgexec.Query(ctx, `DELETE FROM events WHERE id IN (
		SELECT e.id FROM events e WHERE `+removable+` ORDER BY e.id LIMIT $2) RETURNING id::text`, pgexec.Text, cutoff, limit)
}

// HeldQuery narrows a held person's events: words in the title, notes or
// place, and a stretch of time the event touches (zero: unbounded).
type HeldQuery struct {
	Text     string
	From, To time.Time
	// AfterStart and AfterID, when AfterID is set, start the list after that
	// event in its order (the next part of an export).
	AfterStart time.Time
	AfterID    string
}

// HeldEvents is what people organise, own the calendar of or are a guest of,
// deleted ones included: single events and series (their overrides are read
// with them), newest first (by start, then id), at most limit, after
// q.AfterStart and q.AfterID when q.AfterID is set.
func HeldEvents(ctx context.Context, db DB, people []string, q HeldQuery, limit int) ([]*Event, error) {
	from, to := q.From, q.To
	if from.IsZero() {
		from = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if to.IsZero() {
		to = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	var after *string
	if q.AfterID != "" {
		after = &q.AfterID
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		WHERE e.series_id IS NULL
		  AND (e.created_by = ANY($1::uuid[])
		       OR EXISTS (SELECT 1 FROM calendars c WHERE c.id = e.calendar_id AND c.owner_id = ANY($1::uuid[]))
		       OR EXISTS (SELECT 1 FROM event_attendees a WHERE a.user_id = ANY($1::uuid[])
		                  AND (a.event_id = e.id OR a.event_id IN (SELECT o.id FROM events o WHERE o.series_id = e.id))))
		  AND ($2 = '' OR strpos(lower(e.title || ' ' || e.notes || ' ' || e.location), lower($2)) > 0)
		  AND COALESCE(e.start_at, e.start_date::timestamp AT TIME ZONE 'UTC') < $4
		  AND (CASE WHEN e.rrule = '' THEN `+endedAt("e")+`
		            ELSE COALESCE(e.until_at + `+lengthOf+`, 'infinity'::timestamptz) END) >= $3
		  AND ($6::uuid IS NULL
		       OR COALESCE(e.start_at, e.start_date::timestamp AT TIME ZONE 'UTC') < $7
		       OR (COALESCE(e.start_at, e.start_date::timestamp AT TIME ZONE 'UTC') = $7 AND e.id > $6::uuid))
		ORDER BY COALESCE(e.start_at, e.start_date::timestamp AT TIME ZONE 'UTC') DESC, e.id
		LIMIT $5`, pq.Array(people), q.Text, from, to, limit, after, q.AfterStart)
}
