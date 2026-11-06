package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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

// FieldsOfTables is the live fields of the tables among ids, by table, each
// table's in ListFields' order, in one read.
func FieldsOfTables(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]*Field, error) {
	out := map[uuid.UUID][]*Field{}
	if len(ids) == 0 {
		return out, nil
	}
	fields, err := pgexec.Query(ctx, `SELECT `+fieldColumns+` FROM data_table_fields
		WHERE table_id = ANY($1) AND deleted_at IS NULL ORDER BY table_id, position ASC, created_at ASC`,
		func(r pgexec.Row) (*Field, error) { return scanField(r) }, ids)
	for _, f := range fields {
		out[f.TableId] = append(out[f.TableId], f)
	}
	return out, err
}
