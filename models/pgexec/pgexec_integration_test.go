//go:build integration

package pgexec_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

	// Upkeep no transaction may hold runs alone, even from a ctx inside one;
	// Exec runs it in that transaction, where Postgres refuses it.
	var inTx error
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.ExecAlone(ctx, time.Minute, `VACUUM (ANALYZE) pgexec_t`); err != nil {
			return err
		}
		_, inTx = pgexec.Exec(ctx, `VACUUM (ANALYZE) pgexec_t`)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("a VACUUM alone from a ctx in a transaction: %v", err)
	}
	if inTx == nil {
		t.Fatal("a VACUUM ran in the transaction: the ctx carried none, so nothing here was alone")
	}

	// A VACUUM right after commits that don't wait for the disk (this
	// server's synchronous_commit is off, as a self-hosted one's may be)
	// marks every page they wrote whole once FlushCommits ran. Without it the
	// newest 32 transactions' rows have no hint bit yet, and their pages stay
	// unmarked (V-150).
	if s, err := pgexec.Scalar[string](ctx, `SHOW synchronous_commit`); err != nil || s != "off" {
		t.Fatalf("this server's synchronous_commit is %q (%v): the flush below proves nothing unless commits don't wait for the disk", s, err)
	}
	if _, err := pgexec.Exec(ctx, `CREATE TABLE pgexec_w (n int, pad text) WITH (autovacuum_enabled = false)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := pgexec.Exec(ctx, `INSERT INTO pgexec_w SELECT g, repeat('w', 200) FROM generate_series(1, 500) g`); err != nil {
			t.Fatal(err)
		}
	}
	if err := pgexec.FlushCommits(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pgexec.ExecAlone(ctx, time.Minute, `VACUUM pgexec_w`); err != nil {
		t.Fatal(err)
	}
	var whole, pages int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT relallvisible, relpages FROM pg_class WHERE oid = 'pgexec_w'::regclass`).Scan(&whole, &pages); err != nil {
		t.Fatal(err)
	}
	if pages == 0 || whole != pages {
		t.Fatalf("a VACUUM after FlushCommits marked %d of the fill's %d pages whole: a commit not yet on disk kept its rows unhinted", whole, pages)
	}
}
