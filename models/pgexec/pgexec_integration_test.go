//go:build integration

package pgexec_test

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The generics run what they are given on the caller's ctx, in the
// transaction ctx carries, and read every row or fail.
func TestPgexecRunsStatementsAndTransactions(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	if _, err := pgexec.Exec(ctx, `CREATE TABLE IF NOT EXISTS pgexec_t (k text PRIMARY KEY, v int NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	n, err := pgexec.Exec(ctx, `INSERT INTO pgexec_t (k, v) VALUES ('a', 1), ('b', 2), ('c', 3)`)
	if err != nil || n != 3 {
		t.Fatalf("insert: %d rows, %v", n, err)
	}
	type kv struct {
		k string
		v int
	}
	scan := func(r pgexec.Row) (kv, error) {
		var x kv
		err := r.Scan(&x.k, &x.v)
		return x, err
	}
	all, err := pgexec.Query(ctx, `SELECT k, v FROM pgexec_t ORDER BY k`, scan)
	if err != nil || len(all) != 3 || all[2] != (kv{"c", 3}) {
		t.Fatalf("query: %v %v", all, err)
	}
	none, err := pgexec.Query(ctx, `SELECT k, v FROM pgexec_t WHERE v > $1`, scan, 10)
	if err != nil || none != nil {
		t.Fatalf("no rows is a nil slice: %v %v", none, err)
	}
	if _, found, err := pgexec.QueryOne(ctx, `SELECT k, v FROM pgexec_t WHERE k = $1`, scan, "zz"); found || err != nil {
		t.Fatalf("no row: found %v, err %v", found, err)
	}
	if one, found, err := pgexec.QueryOne(ctx, `SELECT k, v FROM pgexec_t WHERE k = $1`, scan, "b"); !found || err != nil || one.v != 2 {
		t.Fatalf("one row: %v %v %v", one, found, err)
	}
	if sum, err := pgexec.Scalar[int](ctx, `SELECT sum(v) FROM pgexec_t`); err != nil || sum != 6 {
		t.Fatalf("scalar: %d %v", sum, err)
	}
	if m, err := pgexec.Pairs[string, int](ctx, `SELECT k, v FROM pgexec_t`); err != nil || m["a"] != 1 || len(m) != 3 {
		t.Fatalf("pairs: %v %v", m, err)
	}

	// A transaction that fails takes back everything in it, an inner InTx's
	// statements included; one that succeeds keeps them.
	boom := errors.New("boom")
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `UPDATE pgexec_t SET v = v + 10`); err != nil {
			return err
		}
		if err := pgexec.InTx(ctx, func(ctx context.Context) error {
			_, err := pgexec.Exec(ctx, `INSERT INTO pgexec_t (k, v) VALUES ('d', 4)`)
			return err
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the failed transaction's error comes back: %v", err)
	}
	if sum, _ := pgexec.Scalar[int](ctx, `SELECT sum(v) FROM pgexec_t`); sum != 6 {
		t.Fatalf("a failed transaction kept its writes: sum %d", sum)
	}
	if err := pgexec.InTx(ctx, func(ctx context.Context) error {
		_, err := pgexec.Exec(ctx, `INSERT INTO pgexec_t (k, v) VALUES ('d', 4)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sum, _ := pgexec.Scalar[int](ctx, `SELECT sum(v) FROM pgexec_t`); sum != 10 {
		t.Fatalf("a committed transaction lost its write: sum %d", sum)
	}

	// A model handed Current's executor inside InTx writes in that
	// transaction: taken back with it, kept when it commits.
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		ex, err := pgexec.Current(ctx)
		if err != nil {
			return err
		}
		if _, err := ex.ExecContext(ctx, `INSERT INTO pgexec_t (k, v) VALUES ('e', 5)`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the failed transaction's error comes back: %v", err)
	}
	if c, _ := pgexec.Scalar[int](ctx, `SELECT count(*) FROM pgexec_t WHERE k = 'e'`); c != 0 {
		t.Fatal("a statement through Current outlived its transaction's rollback")
	}

	// A cancelled caller's statement doesn't run.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pgexec.Exec(cctx, `DELETE FROM pgexec_t`); err == nil {
		t.Fatal("a cancelled ctx ran its statement")
	}
	if c, _ := pgexec.Scalar[int](ctx, `SELECT count(*) FROM pgexec_t`); c != 4 {
		t.Fatalf("the cancelled delete ran: %d rows left", c)
	}
}
