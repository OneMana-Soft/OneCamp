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
//     one it gets the transaction's (InTx); upkeep no transaction may hold
//     gets the bound its caller gives (ExecAlone);
//   - rows are always closed and rows.Err() always read, so a list cut short
//     by a dropped connection is an error, never a short list;
//   - a statement inside InTx runs in that transaction without being handed
//     a *sql.Tx: the transaction travels in ctx (WithTx puts one there for
//     code another executor handed a *sql.Tx).
package pgexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	return context.WithTimeout(ctx, statementTimeout())
}

// Exec runs a statement that returns no rows, and says how many it touched.
// WithoutWorkers run in a transaction marks it (plannedForItsRun).
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
	if tx, ok := d.(*sql.Tx); ok && q == WithoutWorkers {
		markWithoutWorkers(tx)
	}
	return res.RowsAffected()
}

// ExecAlone runs a statement that returns no rows on the pool, never in a
// transaction ctx carries, within d rather than the pool's DBTimeout, and
// says how many rows it touched. It is for upkeep Postgres refuses inside a
// transaction block and that outlasts a request's statement on a large
// table: VACUUM, CREATE INDEX CONCURRENTLY. Exec would run one in ctx's
// transaction, where it fails, or cut it off at DBTimeout, so the table
// models ran their vacuums by hand on the pool (V-150). ctx still ends it:
// a caller that is stopped stops it. d bounds it as context.WithTimeout does.
func ExecAlone(ctx context.Context, d time.Duration, q string, args ...any) (int64, error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return 0, ErrNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FlushCommits returns once every commit made before it is on disk, which a
// VACUUM needs to mark the pages those commits wrote whole. A VACUUM marks a
// page whole only when it can take each of its rows for committed (its hint
// bit), and Postgres sets that bit only once the commit is on disk. A server
// whose commits don't wait for the disk (synchronous_commit off: the
// integration harness's, and a tuning self-hosted servers take) leaves the
// disk to its WAL writer, up to a few fifths of a second later. Its commit log
// keeps one such point for each 32 transactions, so one commit that isn't on
// disk yet holds back the rows of its 31 neighbours too: a VACUUM right after
// a 5,000-row fill marked none of its cell index whole (V-150).
//
// It waits for the server's WAL writer rather than commit one of its own: a
// transaction that writes nothing but its xid commits without waiting for
// the disk, synchronous_commit on or not. It reads how far the log is written
// as it begins (pg_current_wal_insert_lsn), past every commit made before it,
// and polls how far it is on disk (pg_current_wal_flush_lsn) every flushPoll
// until that is as far, on the pool, within d. On a server whose commits wait
// for the disk it is as far at once, or at the WAL writer's next round. A
// record that ended on a page's end puts the written point behind the next
// page's header, which the disk never reaches before more log comes: a log
// on disk to that boundary is as far (flushedPast; V-155: an idle server
// waited ~15 s, past d).
func FlushCommits(ctx context.Context, d time.Duration) error {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return ErrNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	pool := postgresInit.DBConn.SqlDB
	var at string
	var block, segment uint64
	if err := pool.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text, current_setting('wal_block_size')::bigint, pg_size_bytes(current_setting('wal_segment_size'))`).Scan(&at, &block, &segment); err != nil {
		return err
	}
	written, err := parseLSN(at)
	if err != nil {
		return err
	}
	for {
		if err := pool.QueryRowContext(ctx, `SELECT pg_current_wal_flush_lsn()::text`).Scan(&at); err != nil {
			return err
		}
		flushed, err := parseLSN(at)
		if err != nil {
			return err
		}
		if flushedPast(flushed, written, block, segment) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(flushPoll):
		}
	}
}

// flushPoll is how often FlushCommits looks how far the log is on disk.
const flushPoll = 20 * time.Millisecond

// Query runs a statement and reads every row it returns with scan. A
// failure partway (rows.Err) is an error, never a short list. No rows is an
// empty (nil) slice. A read that runs out of shared memory before its first
// row runs once more without parallel workers (againWithoutWorkers).
func Query[T any](ctx context.Context, q string, scan func(Row) (T, error), args ...any) ([]T, error) {
	var out []T
	err := againWithoutWorkers(ctx, q, func(ctx context.Context) (handed bool, err error) {
		out, handed, err = queryOnce(ctx, q, scan, args...)
		return handed, err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// queryOnce is one run of Query's statement; handed says scan was given a row.
func queryOnce[T any](ctx context.Context, q string, scan func(Row) (T, error), args ...any) (out []T, handed bool, err error) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	d, err := db(ctx)
	if err != nil {
		return nil, false, err
	}
	rows, err := d.QueryContext(ctx, q, plannedForItsRun(ctx, args)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		handed = true
		v, err := scan(rows)
		if err != nil {
			return nil, true, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, handed, err
	}
	return out, handed, nil
}

// QueryOne reads the first row a statement returns with scan: found false,
// and no error, when there is none.
func QueryOne[T any](ctx context.Context, q string, scan func(Row) (T, error), args ...any) (T, bool, error) {
	var zero, v T
	// A row's error is Postgres's before the row: Scan hands back none.
	err := againWithoutWorkers(ctx, q, func(ctx context.Context) (bool, error) {
		ctx, cancel := statementCtx(ctx)
		defer cancel()
		d, err := db(ctx)
		if err != nil {
			return false, err
		}
		v, err = scan(d.QueryRowContext(ctx, q, plannedForItsRun(ctx, args)...))
		return false, err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

// Text reads a row of one text column (ids as ::text), for Query.
func Text(r Row) (s string, err error) {
	err = r.Scan(&s)
	return
}

// Scalar reads the one value a statement returns (a count, an id, an
// EXISTS). sql.ErrNoRows when it returns no row.
func Scalar[T any](ctx context.Context, q string, args ...any) (T, error) {
	var v T
	err := againWithoutWorkers(ctx, q, func(ctx context.Context) (bool, error) {
		ctx, cancel := statementCtx(ctx)
		defer cancel()
		d, err := db(ctx)
		if err != nil {
			return false, err
		}
		return false, d.QueryRowContext(ctx, q, plannedForItsRun(ctx, args)...).Scan(&v)
	})
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
	return InTxFor(ctx, TxTimeout, fn)
}

// InTxFor is InTx with budget d: for a transaction longer than a few
// statements (a column's cells rewritten with it, postgresInit.BulkTimeout).
// Inside another InTx it joins that one, under its budget. What fn hands
// AfterCommit runs once the transaction has committed.
func InTxFor(ctx context.Context, d time.Duration, fn func(ctx context.Context) error) error {
	if inTx(ctx) {
		return fn(ctx)
	}
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return ErrNoDatabase
	}
	after := &afterCommit{}
	err := postgresInit.InTx(ctx, d, func(tctx context.Context, tx *sql.Tx) error {
		return fn(context.WithValue(WithTx(tctx, tx), afterKey{}, after))
	})
	if err == nil {
		after.run(ctx)
	}
	return err
}

// afterKey holds the outermost InTx's afterCommit.
type afterKey struct{}

// afterCommit is what a transaction's writes tell others once it commits.
type afterCommit struct {
	mu  sync.Mutex
	fns []func(ctx context.Context)
}

func (a *afterCommit) add(fn func(ctx context.Context)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fns = append(a.fns, fn)
}

// run runs what was handed over, in order, with ctx: InTx's caller's, which
// carries no transaction and isn't cancelled with it.
func (a *afterCommit) run(ctx context.Context) {
	a.mu.Lock()
	fns := a.fns
	a.fns = nil
	a.mu.Unlock()
	for _, fn := range fns {
		fn(ctx)
	}
}

// AfterCommit runs fn once ctx's transaction has committed, so what a write
// tells others (a live message, an index, a webhook) reaches them when a
// reader can find the write, and a write rolled back tells nothing. Inside
// InTx fn waits for the outermost commit and gets InTx's caller's ctx; it is
// dropped if the transaction rolls back. Anywhere else, a bare WithTx
// included (its owner commits), fn runs at once with ctx.
func AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if a, ok := ctx.Value(afterKey{}).(*afterCommit); ok && inTx(ctx) {
		a.add(fn)
		return
	}
	fn(ctx)
}

// WithTx is ctx carrying tx: every pgexec statement made with it runs in tx,
// as one inside InTx does, and an InTx inside it joins tx. It is for code
// another executor handed a *sql.Tx: the row ledger's observers get the
// write's own transaction, and a batch that needs a longer budget than
// TxTimeout opens its own with postgresInit.InTx. The owner of tx commits or
// rolls it back; a statement gets no timeout of its own, since tx's context
// bounds it. A nil tx is ctx as it was.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	if tx == nil {
		return ctx
	}
	return context.WithValue(ctx, txKey{}, tx)
}

// The WAL's page headers on a 64-bit server (MAXALIGN 8): a segment's first
// page carries the long one.
const (
	shortPageHeader = 24
	longPageHeader  = 40
)

// flushedPast is whether a log on disk to flushed holds everything written
// before written, on a server whose WAL pages are block bytes and segments
// segment bytes: as far, or exactly at a page boundary written is only that
// page's header past (pg_current_wal_insert_lsn says where the next record
// goes, behind the header once a record ended on the page's end; the disk
// stops at the boundary). Any other gap is log not on disk yet. Pure.
func flushedPast(flushed, written, block, segment uint64) bool {
	if flushed >= written {
		return true
	}
	if block == 0 || flushed%block != 0 {
		return false
	}
	header := uint64(shortPageHeader)
	if segment != 0 && flushed%segment == 0 {
		header = longPageHeader
	}
	return written-flushed == header
}

// parseLSN reads a WAL position as Postgres writes one: its high and low 32
// bits in hex, "16/B374D848". Pure.
func parseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(s, "/")
	h, herr := strconv.ParseUint(hi, 16, 32)
	l, lerr := strconv.ParseUint(lo, 16, 32)
	if !ok || herr != nil || lerr != nil {
		return 0, fmt.Errorf("pgexec: %q isn't a WAL position", s)
	}
	return h<<32 | l, nil
}
