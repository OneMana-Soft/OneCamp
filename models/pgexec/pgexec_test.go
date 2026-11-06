package pgexec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// ran is a statement the recorder was given: its text, whether its
// connection was in a transaction, and whether its ctx had a deadline.
type ran struct {
	q              string
	inTx, deadline bool
}

// recorder is a database/sql connector whose connections record the
// statements they are given and run none, and the time each statement with
// a deadline had left (left).
type recorder struct {
	log  []ran
	left []time.Duration
}

func (r *recorder) Connect(context.Context) (driver.Conn, error) { return &recConn{r: r}, nil }
func (r *recorder) Driver() driver.Driver                        { return recDriver{} }

type recDriver struct{}

func (recDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("recorder: open it as a connector")
}

// recConn is one connection, and its transaction.
type recConn struct {
	r    *recorder
	inTx bool
}

func (c *recConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recorder: no prepared statements")
}
func (c *recConn) Close() error              { return nil }
func (c *recConn) Begin() (driver.Tx, error) { c.inTx = true; return c, nil }
func (c *recConn) Commit() error             { c.inTx = false; return nil }
func (c *recConn) Rollback() error           { c.inTx = false; return nil }
func (c *recConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	at, deadline := ctx.Deadline()
	c.r.log = append(c.r.log, ran{q: q, inTx: c.inTx, deadline: deadline})
	if deadline {
		c.r.left = append(c.r.left, time.Until(at))
	}
	return driver.RowsAffected(1), nil
}

// A transaction another executor hands over runs pgexec's statements.
//
// WHY. pgexec carried a transaction in ctx only inside its own InTx, so code
// handed a *sql.Tx (a row ledger observer, a batch opened with a longer
// budget than TxTimeout) ran its statements by hand on that tx. WithTx puts
// the tx in ctx: a statement made with it runs in tx with tx's own bound, an
// InTx inside it joins tx rather than opening one, and once tx's owner ends
// it a statement is refused rather than run on the pool.
func TestWithTxRunsStatementsInTheHandedTransaction(t *testing.T) {
	ctx := context.Background()
	if WithTx(ctx, nil) != ctx {
		t.Fatal("WithTx with no transaction changed ctx")
	}
	rec := &recorder{}
	pool := sql.OpenDB(rec)
	defer pool.Close()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := WithTx(ctx, tx)
	if n, err := Exec(in, "UPDATE handed"); err != nil || n != 1 {
		t.Fatalf("a statement in the handed transaction: %d rows, %v", n, err)
	}
	// postgresInit has no pool in a unit test: an InTx that opened a
	// transaction of its own would fail with ErrNoDatabase.
	if err := InTx(in, func(ctx context.Context) error {
		_, err := Exec(ctx, "UPDATE joined")
		return err
	}); err != nil {
		t.Fatalf("InTx inside WithTx opened a transaction of its own: %v", err)
	}
	want := []ran{{q: "UPDATE handed", inTx: true}, {q: "UPDATE joined", inTx: true}}
	if !slices.Equal(rec.log, want) {
		t.Fatalf("the driver ran %+v, want %+v (in the transaction, with no deadline of their own)", rec.log, want)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(in, "UPDATE after"); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("a statement after the transaction ended: %v, want sql.ErrTxDone", err)
	}
	if len(rec.log) != len(want) {
		t.Fatalf("a statement after the transaction ended ran: %+v", rec.log[len(want):])
	}
}

// Upkeep runs alone: on the pool, outside the transaction ctx carries, within
// the bound its caller gives.
//
// WHY. Postgres refuses a VACUUM inside a transaction block, and a vacuum of
// a filled table outlasts a request's DBTimeout, so the table models ran
// theirs by hand on the pool, each its own executor (V-150). ExecAlone runs
// one on the pool even from a ctx inside a transaction, bounded by its
// caller's d rather than DBTimeout; with no pool it is refused, never a nil
// pointer.
func TestExecAloneRunsOnThePoolWithinItsCallersBound(t *testing.T) {
	ctx := context.Background()
	was := postgresInit.DBConn
	t.Cleanup(func() { postgresInit.DBConn = was })
	postgresInit.DBConn = &postgresInit.DB{}
	if _, err := ExecAlone(ctx, time.Hour, "VACUUM nowhere"); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("with no pool: %v, want ErrNoDatabase", err)
	}

	rec := &recorder{}
	pool := sql.OpenDB(rec)
	defer pool.Close()
	postgresInit.DBConn = &postgresInit.DB{SqlDB: pool, DBTimeout: time.Second}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if n, err := ExecAlone(WithTx(ctx, tx), time.Hour, "VACUUM alone"); err != nil || n != 1 {
		t.Fatalf("upkeep from a ctx in a transaction: %d rows, %v", n, err)
	}
	want := []ran{{q: "VACUUM alone", deadline: true}}
	if !slices.Equal(rec.log, want) {
		t.Fatalf("the driver ran %+v, want %+v (on the pool, outside the transaction, with a deadline)", rec.log, want)
	}
	if rec.left[0] < 59*time.Minute {
		t.Fatalf("upkeep given an hour had %v left: bounded by the pool's DBTimeout (1s), not its caller's", rec.left[0])
	}
}

// A VACUUM after a fill first has the fill's commits on disk (FlushCommits);
// with no pool it is refused, never a nil pointer.
//
// WHY. A VACUUM marks a page whole only once the commits that wrote it are on
// disk, and on a server whose commits don't wait for the disk a 5,000-row
// fill's cell index had none of its pages marked (V-150). Its wait on a real
// server is proven in pgexec_integration_test.go (a VACUUM after commits
// that didn't wait marks every page once it returns).
func TestFlushCommitsIsRefusedWithNoPool(t *testing.T) {
	was := postgresInit.DBConn
	t.Cleanup(func() { postgresInit.DBConn = was })
	postgresInit.DBConn = &postgresInit.DB{}
	if err := FlushCommits(context.Background(), time.Hour); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("with no pool: %v, want ErrNoDatabase", err)
	}
}

// Past a page's end, the server says the log is written to just behind the
// next page's header (pg_current_wal_insert_lsn), while the disk stops at the
// boundary: a log on disk to the boundary holds everything written then. A
// header is 40 bytes at a segment's start and 24 at any other page's; any
// other gap is log not on disk yet (V-155).
func TestAFlushOnAPageBoundaryHoldsWhatWasWritten(t *testing.T) {
	const block, segment = 8192, 16 << 20
	page := uint64(0x16_0000_0000 + 5*block) // a page boundary inside a segment
	seg := uint64(0x16_0000_0000 + 2*segment)
	cases := []struct {
		flushed, written uint64
		want             bool
	}{
		{page, page, true},
		{page + 8, page, true},
		{page, page + 24, true},
		{page, page + 40, false},
		{page, page + 41, false},
		{page, page + 16, false},
		{page + 8, page + 32, false},
		{seg, seg + 40, true},
		{seg, seg + 24, false},
		{seg - block, seg + 40, false},
	}
	for _, c := range cases {
		if got := flushedPast(c.flushed, c.written, block, segment); got != c.want {
			t.Errorf("flushedPast(%X, %X) = %v, want %v", c.flushed, c.written, got, c.want)
		}
	}
	for s, want := range map[string]uint64{"0/0": 0, "16/B374D848": 0x16_B374_D848, "0/1000000": 1 << 24} {
		if got, err := parseLSN(s); err != nil || got != want {
			t.Errorf("parseLSN(%q) = %X, %v; want %X", s, got, err, want)
		}
	}
	for _, s := range []string{"", "16", "x/1", "1/y", "1/2/3"} {
		if _, err := parseLSN(s); err == nil {
			t.Errorf("parseLSN(%q) took it", s)
		}
	}
}

// What a write tells others waits for its transaction's commit.
//
// WHY. An API item's write and its audit row share one transaction (main's
// audit rule, 3 Nov 2026), so the write runs inside one. A live message, an
// index entry or a webhook sent from inside it would reach readers before
// the write could be found, and one sent for a write that then rolled back
// would tell of a change that never happened. AfterCommit holds them until
// the outermost commit, hands them a ctx with no transaction that isn't
// cancelled with it, and drops them on a rollback; outside InTx, and under a
// bare WithTx (whose owner commits), it runs them at once.
func TestAfterCommitWaitsForTheOutermostCommit(t *testing.T) {
	was := postgresInit.DBConn
	t.Cleanup(func() { postgresInit.DBConn = was })
	rec := &recorder{}
	pool := sql.OpenDB(rec)
	defer pool.Close()
	postgresInit.DBConn = &postgresInit.DB{SqlDB: pool, DBTimeout: time.Second}

	var told []string
	tell := func(what string) func(context.Context) {
		return func(ctx context.Context) {
			if inTx(ctx) || ctx.Err() != nil {
				what += " (in a transaction, or cancelled)"
			}
			told = append(told, what)
		}
	}
	ctx := context.Background()
	AfterCommit(ctx, tell("at once"))
	err := InTxFor(ctx, time.Second, func(ctx context.Context) error {
		AfterCommit(ctx, tell("outer"))
		if err := InTx(ctx, func(ctx context.Context) error {
			AfterCommit(ctx, tell("nested"))
			return nil
		}); err != nil {
			return err
		}
		if !slices.Equal(told, []string{"at once"}) {
			t.Errorf("before the commit, told %v; want only what ran outside", told)
		}
		return nil
	})
	if err != nil || !slices.Equal(told, []string{"at once", "outer", "nested"}) {
		t.Fatalf("after the commit: %v, told %v; want at once, outer, nested, each with no transaction", err, told)
	}

	told = nil
	boom := errors.New("the write failed")
	err = InTx(ctx, func(ctx context.Context) error {
		AfterCommit(ctx, tell("rolled back"))
		return boom
	})
	if !errors.Is(err, boom) || len(told) != 0 {
		t.Fatalf("a rolled-back transaction: %v, told %v; want its error and nothing told", err, told)
	}

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	AfterCommit(WithTx(ctx, tx), func(context.Context) { told = append(told, "handed") })
	if !slices.Equal(told, []string{"handed"}) {
		t.Fatalf("under a bare WithTx, told %v; want it at once", told)
	}
}
