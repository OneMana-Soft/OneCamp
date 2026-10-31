package pgexec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"testing"
)

// ran is a statement the recorder was given: its text, whether its
// connection was in a transaction, and whether its ctx had a deadline.
type ran struct {
	q              string
	inTx, deadline bool
}

// recorder is a database/sql connector whose connections record the
// statements they are given and run none.
type recorder struct{ log []ran }

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
	_, deadline := ctx.Deadline()
	c.r.log = append(c.r.log, ran{q: q, inTx: c.inTx, deadline: deadline})
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
