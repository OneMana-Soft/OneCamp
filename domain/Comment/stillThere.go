package domain

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// StillThere is which of the comments named (by uuid) are still there, not
// deleted: a member's (comments) or a guest's on a doc (guest_comments), by
// lower-case uuid, in one read. An id that isn't a uuid names none.
func StillThere(ctx context.Context, commentUUIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	ids := make([]string, 0, len(commentUUIDs))
	for _, id := range commentUUIDs {
		if u, err := uuid.Parse(id); err == nil {
			ids = append(ids, u.String())
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	there, err := pgexec.Query(ctx, `
		SELECT a.id::text FROM unnest($1::uuid[]) AS a(id)
		WHERE EXISTS (SELECT 1 FROM comments c WHERE c.id = a.id AND c.deleted_at IS NULL)
			OR EXISTS (SELECT 1 FROM guest_comments g WHERE g.id = a.id AND g.deleted_at IS NULL)`,
		func(r pgexec.Row) (id string, err error) {
			err = r.Scan(&id)
			return id, err
		}, pgexec.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, id := range there {
		out[id] = true
	}
	return out, nil
}
