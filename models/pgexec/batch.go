package pgexec

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Read is one statement of a Batch: its text and arguments (from domain/, or
// the model that keeps them), and what reads each row it returns. Err is the
// read's own error once Batch has run it.
type Read struct {
	stmt  Stmt
	start func() // before each run: forget what a failed run read
	scan  func(Row) error
	Err   error
}

// Rows is a Read of every row q returns, each read with scan onto *out.
func Rows[T any](out *[]T, scan func(Row) (T, error), q string, args ...any) *Read {
	return &Read{
		stmt:  Stmt{SQL: q, Args: args},
		start: func() { *out = nil },
		scan: func(r Row) error {
			v, err := scan(r)
			if err != nil {
				return err
			}
			*out = append(*out, v)
			return nil
		},
	}
}

// Batch runs reads that don't wait on one another in one round trip: pgx's
// pipeline (SendBatch), reached under database/sql through sql.Conn.Raw.
// Each read keeps its own rows and its own error: pgx ends a batch at one
// Sync, so a failure would fail the reads after it with it; the failed read
// runs again alone, for its own answer, and those after it go in a batch of
// their own. Inside a transaction (InTx, WithTx) the reads run on it, one
// after another: database/sql gives no way to a transaction's connection, so
// there they can't share a round trip. A driver that isn't pgx's (a test's)
// runs them one after another too.
func Batch(ctx context.Context, reads ...*Read) {
	reads = slices.DeleteFunc(slices.Clone(reads), func(r *Read) bool { return r == nil })
	if len(reads) == 0 {
		return
	}
	if len(reads) == 1 || inTx(ctx) {
		inTurn(ctx, reads)
		return
	}
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		for _, r := range reads {
			r.Err = ErrNoDatabase
		}
		return
	}
	failed, piped := pipelined(ctx, reads)
	switch {
	case !piped:
		inTurn(ctx, reads)
	case failed >= 0:
		reads[failed].run(ctx)
		Batch(ctx, reads[failed+1:]...)
	}
}

// pipelined sends reads as one pgx batch on a connection of the pool, and
// says which failed first (-1 for none), or that the pool's driver isn't
// pgx's (piped false; nothing was sent). Every read up to the failed one
// has its answer.
func pipelined(ctx context.Context, reads []*Read) (failed int, piped bool) {
	ctx, cancel := statementCtx(ctx)
	defer cancel()
	failed = -1
	conn, err := postgresInit.DBConn.SqlDB.Conn(ctx)
	if err != nil {
		for _, r := range reads {
			r.Err = err
		}
		return failed, true
	}
	defer func() { _ = conn.Close() }()
	err = conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(interface{ Conn() *pgx.Conn })
		if !ok {
			return nil
		}
		piped = true
		b := &pgx.Batch{}
		for _, r := range reads {
			b.Queue(r.stmt.SQL, plannedForItsRun(ctx, r.stmt.Args)...)
		}
		results := c.Conn().SendBatch(ctx, b)
		for i, r := range reads {
			if r.Err = r.readFrom(results); r.Err != nil {
				failed = i
				break
			}
		}
		// The reads all answered; a failure past them is the connection's,
		// which the pool drops.
		_ = results.Close()
		return nil
	})
	if err != nil { // the connection was gone before anything was sent
		for _, r := range reads {
			r.Err = err
		}
		return -1, true
	}
	return failed, piped
}

// readFrom reads the read's rows, the next answer in results.
func (r *Read) readFrom(results pgx.BatchResults) error {
	r.start()
	rows, err := results.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := r.scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// inTurn runs each read on its own, in order.
func inTurn(ctx context.Context, reads []*Read) {
	for _, r := range reads {
		r.run(ctx)
	}
}

// run runs the read on its own, as Query runs a statement.
func (r *Read) run(ctx context.Context) {
	r.Err = againWithoutWorkers(ctx, r.stmt.SQL, func(ctx context.Context) (bool, error) {
		r.start()
		_, handed, err := queryOnce(ctx, r.stmt.SQL, func(row Row) (struct{}, error) { return struct{}{}, r.scan(row) }, r.stmt.Args...)
		return handed, err
	})
}

// Planned is reads a caller hands to Together beside other callers', and
// what it does with them once they have run (a read that waits on them goes
// there).
type Planned struct {
	Reads []*Read
	Then  func()
}

// Together runs every plan's reads in one Batch, then each plan's Then, in
// the order given.
func Together(ctx context.Context, plans ...Planned) {
	var reads []*Read
	for _, p := range plans {
		reads = append(reads, p.Reads...)
	}
	Batch(ctx, reads...)
	for _, p := range plans {
		if p.Then != nil {
			p.Then()
		}
	}
}
