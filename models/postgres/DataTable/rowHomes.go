package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// RowHome is where a row lives: its table, and whether it is in the trash.
type RowHome struct {
	Table   uuid.UUID
	Deleted bool
}

// RowHomes is, for the rows among ids, deleted ones too, which table each is
// in: what a decision about rows by id alone (a thread on a row, a notice
// about one) reads first. A row that isn't there is left out. The ids go to
// pgx as they are (CellsOf says why).
func RowHomes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]RowHome, error) {
	out := make(map[uuid.UUID]RowHome, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT id, table_id, deleted_at IS NOT NULL FROM data_table_rows WHERE id = ANY($1)`, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowHomes err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var h RowHome
		if err := rows.Scan(&id, &h.Table, &h.Deleted); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}
