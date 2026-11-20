// Package domain (Departed) is the query text a handover of a departed
// member's work reads, run through models/pgexec.
package domain

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// handoverPeopleQuery is one read: rows of (event, person) for the people of
// $1 still here (event NULL), then each live event of $2's people still here,
// its organiser first and its members in the order they were added. Still
// here is departedModel.ActiveMember's rule.
const handoverPeopleQuery = `
	WITH here AS (
		SELECT id FROM users WHERE deleted_at IS NULL AND is_bot = false AND is_external = false
	)
	SELECT NULL::uuid AS event, h.id AS person, 0 AS ord, NULL::timestamptz AS added
		FROM here h WHERE h.id = ANY($1::uuid[])
	UNION ALL
	SELECT e.id, e.created_by, 0, NULL
		FROM events e JOIN here h ON h.id = e.created_by
		WHERE e.id = ANY($2::uuid[]) AND e.deleted_at IS NULL
	UNION ALL
	SELECT e.id, a.user_id, 1, a.added_at
		FROM events e JOIN event_attendees a ON a.event_id = e.id JOIN here h ON h.id = a.user_id
		WHERE e.id = ANY($2::uuid[]) AND e.deleted_at IS NULL AND a.user_id <> e.created_by
	ORDER BY 1, 3, 4`

// HandoverPeople answers, in one read, which of people are still active
// members of the workspace, and for each of events (not in the trash) its
// active people: the organiser first, then each invited member in the order
// they were added.
func HandoverPeople(ctx context.Context, people, events []uuid.UUID) (map[uuid.UUID]bool, map[uuid.UUID][]uuid.UUID, error) {
	type row struct{ event, person uuid.NullUUID }
	rows, err := pgexec.Query(ctx, handoverPeopleQuery, func(r pgexec.Row) (row, error) {
		var x row
		var ord int
		var added any
		err := r.Scan(&x.event, &x.person, &ord, &added)
		return x, err
	}, people, events)
	if err != nil {
		return nil, nil, err
	}
	active := map[uuid.UUID]bool{}
	eventPeople := map[uuid.UUID][]uuid.UUID{}
	for _, x := range rows {
		switch {
		case !x.person.Valid:
		case !x.event.Valid:
			active[x.person.UUID] = true
		default:
			eventPeople[x.event.UUID] = append(eventPeople[x.event.UUID], x.person.UUID)
		}
	}
	return active, eventPeople, nil
}
