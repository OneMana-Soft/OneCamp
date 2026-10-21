package models

// Memory items dismissed or detached many at a time (backend audit U2b).

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// SoftDeleteMany is SoftDelete for several items in one statement: the ids
// it dismissed (those that were there and not already dismissed).
func SoftDeleteMany(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `
		UPDATE workspace_memory_items SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL
		RETURNING id`, func(r pgexec.Row) (id uuid.UUID, err error) { err = r.Scan(&id); return }, pq.Array(ids))
}

// ClearOwners is ClearOwner for several items in one statement.
func ClearOwners(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		UPDATE workspace_memory_items SET owner_user_id = NULL
		WHERE id = ANY($1::uuid[]) AND owner_user_id IS NOT NULL AND deleted_at IS NULL`, pq.Array(ids))
	return err
}
