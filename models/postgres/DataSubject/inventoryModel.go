// Package models is the data-subject inventory's Postgres reads: which
// columns name a person, and how many rows of a table name them (backend
// audit U4: business/DataSubject ran them itself).
package models

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// UserColumn is a column that names a person, and its table.
type UserColumn struct{ Table, Column string }

// UserColumns is the uuid columns of the current schema's base tables named
// one of names, at most limit, by table and column: a view would
// double-count its table, and a non-uuid column of the same name is
// something else wearing the same word.
func UserColumns(ctx context.Context, names []string, limit int) ([]UserColumn, error) {
	return pgexec.Query(ctx, `
		SELECT c.table_name, c.column_name
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		  AND c.data_type = 'uuid'
		  AND c.column_name = ANY($1)
		ORDER BY c.table_name, c.column_name
		LIMIT $2`, func(r pgexec.Row) (c UserColumn, err error) { err = r.Scan(&c.Table, &c.Column); return }, pq.Array(names), limit)
}

// CountRowsFor counts one table's rows naming a person in one column. The
// table and column come from the catalog and are quoted all the same; the
// person is a bound parameter.
func CountRowsFor(ctx context.Context, table, column string, userID uuid.UUID) (int64, error) {
	return pgexec.Scalar[int64](ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`,
		pq.QuoteIdentifier(table), pq.QuoteIdentifier(column)), userID)
}
