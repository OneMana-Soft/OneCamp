package models

import (
	"context"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// A deleted view waits in its table's trash (DeleteView sets deleted_at), as
// a deleted field does, until it is brought back (RestoreViews).

// TrashedViews is a table's deleted views that reader would see were they
// live (the shared ones, and reader's own), newest first, at most limit.
func TrashedViews(ctx context.Context, table, reader uuid.UUID, limit int) ([]*View, error) {
	return pgexec.Query(ctx, `SELECT `+viewColumns+` FROM data_table_views
		WHERE table_id=$1 AND deleted_at IS NOT NULL AND (owner_id IS NULL OR owner_id=$2)
		ORDER BY deleted_at DESC, id DESC LIMIT $3`, func(r pgexec.Row) (*View, error) { return scanView(r) }, table, reader, limit)
}

// TrashedViewsByIDs is the table's deleted views among ids, in one read.
func TrashedViewsByIDs(ctx context.Context, table uuid.UUID, ids []uuid.UUID) ([]*View, error) {
	return viewsByIDs(ctx, table, ids, "IS NOT NULL")
}

// ViewsByIDs is the table's live views among ids, in one read: what a views
// batch checks its updates and deletes against.
func ViewsByIDs(ctx context.Context, table uuid.UUID, ids []uuid.UUID) ([]*View, error) {
	return viewsByIDs(ctx, table, ids, "IS NULL")
}

// viewsByIDs is the table's views among ids whose deleted_at is as deleted
// says ("IS NULL": live; "IS NOT NULL": in the trash).
func viewsByIDs(ctx context.Context, table uuid.UUID, ids []uuid.UUID, deleted string) ([]*View, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT `+viewColumns+` FROM data_table_views
		WHERE table_id=$1 AND id = ANY($2) AND deleted_at `+deleted, func(r pgexec.Row) (*View, error) { return scanView(r) }, table, ids)
}

// RestoreViews brings the table's deleted views among ids back, as they
// were, in one write, and says how many came back.
func RestoreViews(ctx context.Context, table uuid.UUID, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return pgexec.Exec(ctx, `UPDATE data_table_views SET deleted_at=NULL, updated_at=NOW()
		WHERE table_id=$1 AND id = ANY($2) AND deleted_at IS NOT NULL`, table, ids)
}

// HeldFields is which of ids the table has as fields, live or in its trash,
// in one read: an API batch finds the creates it already made by the ids it
// gave them.
func HeldFields(ctx context.Context, table uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return heldIDs(ctx, `SELECT id FROM data_table_fields WHERE table_id=$1 AND id = ANY($2)`, table, ids)
}

// HeldViews is HeldFields for views.
func HeldViews(ctx context.Context, table uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return heldIDs(ctx, `SELECT id FROM data_table_views WHERE table_id=$1 AND id = ANY($2)`, table, ids)
}

func heldIDs(ctx context.Context, q string, table uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	held := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return held, nil
	}
	got, err := pgexec.Query(ctx, q, scanID, table, ids)
	for _, id := range got {
		held[id] = true
	}
	return held, err
}
