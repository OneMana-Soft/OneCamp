// Package pgexec is the one way a model runs SQL: statements come from the
// domain (or a model's own constants) as text and arguments, and these
// generics run them with the caller's context, the pool's statement timeout,
// and the transaction the context carries, if any.
//
// WHY. Before this every model function was its own executor: about 890 of
// them, 135 of them "take a string, ExecContext, log" copies, 150 with no ctx
// parameter that ran on context.Background(), 24 copies of a scanner
// interface, and 41 raw BeginTx calls. Each one decided again whether to
// check rows.Err(), whether to time out, and whether a cancelled request
// cancels the query. Here it is decided once:
//
//   - the caller's ctx is used, so a request that ends ends its query;
//   - outside a transaction a statement gets postgresInit's DBTimeout; inside
//     one it gets the transaction's (InTx);
//   - rows are always closed and rows.Err() always read, so a list cut short
//     by a dropped connection is an error, never a short list;
//   - a statement inside InTx runs in that transaction without being handed
//     a *sql.Tx: the transaction travels in ctx.
package pgexec

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Row is what a scan function reads one row from: *sql.Row and *sql.Rows
// both are one.
type Row interface {
	Scan(dest ...any) error
}

// Querier is what runs a statement: the pool, or a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Stmt is a statement and its arguments, as a domain function returns one
// for a model to run.
type Stmt struct {
	SQL  string
	Args []any
}

type txKey struct{}

// TxTimeout is how long InTx gives a transaction of a few statements.
const TxTimeout = 10 * time.Second

// ErrNoDatabase is a statement with no database to run on.
var ErrNoDatabase = errors.New("pgexec: postgres is not connected")

// db is what a statement in ctx runs on: the transaction InTx put in ctx, or
// the pool.
func db(ctx context.Context) (Querier, error) {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok && tx != nil {
		return tx, nil
	}
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, ErrNoDatabase
	}
	return postgresInit.DBConn.SqlDB, nil
}

// inTx reports whether ctx carries a transaction.
func inTx(ctx context.Context) bool {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	return ok && tx != nil
}

// statementCtx is ctx with the statement timeout: the pool's DBTimeout
// outside a transaction; inside one the transaction already has its own.
func statementCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if inTx(ctx) {
		return ctx, func() {}
	}
	d := postgresInit.DBConn.DBTimeout
	if d <= 0 {
		d = 3 * time.Second
	}
	return context.WithTimeout(ctx, d)
}

// Exec runs a statement that returns no rows, and says how many it touched.
func Exec(ctx context.Context, q string, args ...any) (int64, error) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	d, err := db(ctx)
	if err != nil {
		return 0, err
	}
	res, err := d.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Query runs a statement and reads every row it returns with scan. A
// failure partway (rows.Err) is an error, never a short list. No rows is an
// empty (nil) slice.
func Query[T any](ctx context.Context, q string, scan func(Row) (T, error), args ...any) ([]T, error) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	d, err := db(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryOne reads the first row a statement returns with scan: found false,
// and no error, when there is none.
func QueryOne[T any](ctx context.Context, q string, scan func(Row) (T, error), args ...any) (T, bool, error) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	var zero T
	d, err := db(ctx)
	if err != nil {
		return zero, false, err
	}
	v, err := scan(d.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

// Scalar reads the one value a statement returns (a count, an id, an
// EXISTS). sql.ErrNoRows when it returns no row.
func Scalar[T any](ctx context.Context, q string, args ...any) (T, error) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	var v T
	d, err := db(ctx)
	if err != nil {
		return v, err
	}
	err = d.QueryRowContext(ctx, q, args...).Scan(&v)
	return v, err
}

// Pairs reads a two-column statement into a map from the first column to the
// second. A key returned twice keeps its last value.
func Pairs[K comparable, V any](ctx context.Context, q string, args ...any) (map[K]V, error) {
	type kv struct {
		k K
		v V
	}
	rows, err := Query(ctx, q, func(r Row) (kv, error) {
		var p kv
		err := r.Scan(&p.k, &p.v)
		return p, err
	}, args...)
	if err != nil {
		return nil, err
	}
	out := make(map[K]V, len(rows))
	for _, p := range rows {
		out[p.k] = p.v
	}
	return out, nil
}

// Current is what a statement made with ctx runs on: the transaction InTx put
// in ctx, or the pool. It is for a model that takes its executor as an
// argument (business/Outbox's Add and Cancel) to join InTx's transaction.
func Current(ctx context.Context) (Querier, error) {
	return db(ctx)
}

// InTx runs fn in one transaction, which every pgexec statement fn makes with
// the ctx it is given runs in, and commits when fn returns nil (otherwise it
// rolls back). Inside another InTx it joins that transaction: fn's statements
// commit or roll back with the outer one. TxTimeout bounds it.
func InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if inTx(ctx) {
		return fn(ctx)
	}
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return ErrNoDatabase
	}
	return postgresInit.InTx(ctx, TxTimeout, func(ctx context.Context, tx *sql.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}
