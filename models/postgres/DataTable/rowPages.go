package models

// A row's page (migration 307): the doc a row hosts (docs D-18), made the
// first time someone writes in it. The doc names its row in the graph; this
// is the row's side of the link.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// PagesOfRows is, of the rows among ids (deleted ones too), the page each
// that has one hosts. A row with none, or that isn't there, is left out.
func PagesOfRows(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT id, doc_uuid FROM data_table_rows WHERE id = ANY($1) AND doc_uuid IS NOT NULL`, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PagesOfRows err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row, doc uuid.UUID
		if err := rows.Scan(&row, &doc); err != nil {
			return nil, err
		}
		out[row] = doc
	}
	return out, rows.Err()
}

// PagesOfTable is every page a table's rows host, deleted rows' too: what is
// told when who may see the table changes.
func PagesOfTable(ctx context.Context, table uuid.UUID) ([]uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT doc_uuid FROM data_table_rows WHERE table_id = $1 AND doc_uuid IS NOT NULL`, table)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PagesOfTable err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var doc uuid.UUID
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// EnsureRowPage is a live row's page, made with makePage when the row has none:
// the row is held (FOR UPDATE) while it is made and written, so two asking
// at once get one page. made says whether makePage was called and its page
// kept. sql.ErrNoRows for a row that isn't in the table, or is in the trash.
func EnsureRowPage(ctx context.Context, table, row uuid.UUID, makePage func(context.Context) (uuid.UUID, error)) (page uuid.UUID, made bool, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return uuid.Nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var have uuid.NullUUID
	err = tx.QueryRowContext(dbctx,
		`SELECT doc_uuid FROM data_table_rows WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE`,
		row, table).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, sql.ErrNoRows
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/EnsureRowPage %s read err: %+v", row, err)
		return uuid.Nil, false, err
	}
	if have.Valid {
		return have.UUID, false, nil
	}
	page, err = makePage(dbctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	if _, err = tx.ExecContext(dbctx, `UPDATE data_table_rows SET doc_uuid = $1 WHERE id = $2`, page, row); err != nil {
		helpers.LogErrorWithContext(ctx, "models/EnsureRowPage %s write err: %+v", row, err)
		return uuid.Nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return uuid.Nil, false, err
	}
	return page, true, nil
}
