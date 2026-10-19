package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// TablesByIDs is the tables among ids, deleted ones too (their DeletedAt
// says so), in no particular order: what a decision about many tables reads
// at once. The ids go to pgx as they are (CellsOf says why).
func TablesByIDs(ctx context.Context, ids []uuid.UUID) ([]*DataTable, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT `+tableColumns+` FROM data_tables WHERE id = ANY($1)`, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TablesByIDs err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := make([]*DataTable, 0, len(ids))
	for rows.Next() {
		t, scanErr := scanTable(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
