//go:build integration

package pgexec_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/akashc777/OneCamp/tests/integration"
)

// tripCounter counts what reaches Postgres: a statement, or a pipelined batch.
type tripCounter struct{ statements, batches atomic.Int64 }

func (c *tripCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.statements.Add(1)
	return ctx
}
func (*tripCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (c *tripCounter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	c.batches.Add(1)
	return ctx
}
func (*tripCounter) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (*tripCounter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func (c *tripCounter) trips() int64 { return c.statements.Load() + c.batches.Load() }

// Batch pipelines independent reads in one round trip; each read keeps its
// own rows and its own error; inside InTx it reads the transaction.
func TestBatchPipelinesReadsAndKeepsEachOnesError(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	counter := &tripCounter{}
	cfg.Tracer = counter
	db := stdlib.OpenDB(*cfg)
	live := postgresInit.DBConn.SqlDB
	postgresInit.DBConn.SqlDB = db
	t.Cleanup(func() { postgresInit.DBConn.SqlDB = live; _ = db.Close() })
	if _, err := pgexec.Exec(ctx, `CREATE TABLE IF NOT EXISTS pgexec_batch (k text PRIMARY KEY, v int NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pgexec.Exec(ctx, `INSERT INTO pgexec_batch (k, v) VALUES ('a', 1), ('b', 2), ('c', 3) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	ints := func(r pgexec.Row) (int, error) {
		var v int
		return v, r.Scan(&v)
	}
	texts := pgexec.Text

	t.Run("N reads are one round trip", func(t *testing.T) {
		var vs []int
		var ks []string
		var n []int
		reads := []*pgexec.Read{
			pgexec.Rows(&vs, ints, `SELECT v FROM pgexec_batch WHERE k = ANY($1) ORDER BY v`, []string{"a", "c"}),
			pgexec.Rows(&ks, texts, `SELECT k FROM pgexec_batch WHERE v > $1 ORDER BY k`, 1),
			pgexec.Rows(&n, ints, `SELECT count(*)::int FROM pgexec_batch`),
		}
		before := counter.trips()
		pgexec.Batch(ctx, reads...)
		if got := counter.trips() - before; got != 1 {
			t.Errorf("3 reads took %d round trips, want 1", got)
		}
		for i, r := range reads {
			if r.Err != nil {
				t.Errorf("read %d: %v", i, r.Err)
			}
		}
		if len(vs) != 2 || vs[0] != 1 || vs[1] != 3 || len(ks) != 2 || ks[0] != "b" || len(n) != 1 || n[0] != 3 {
			t.Errorf("rows: %v %v %v", vs, ks, n)
		}
	})

	t.Run("a read that fails is the only one that fails", func(t *testing.T) {
		for _, bad := range []struct {
			q    string
			args []any
		}{
			{`SELECT 1 / ($1::int - 1)`, []any{1}}, // fails as it runs
			{`SELECT v FROM pgexec_nowhere`, nil},  // fails as it is prepared
		} {
			var first, broken, last []int
			reads := []*pgexec.Read{
				pgexec.Rows(&first, ints, `SELECT v FROM pgexec_batch WHERE k = $1`, "a"),
				pgexec.Rows(&broken, ints, bad.q, bad.args...),
				pgexec.Rows(&last, ints, `SELECT v FROM pgexec_batch WHERE k = $1`, "c"),
			}
			pgexec.Batch(ctx, reads...)
			if reads[1].Err == nil {
				t.Errorf("%s: no error", bad.q)
			}
			if reads[0].Err != nil || reads[2].Err != nil || len(first) != 1 || first[0] != 1 || len(last) != 1 || last[0] != 3 {
				t.Errorf("%s: the others: %v %v, rows %v %v", bad.q, reads[0].Err, reads[2].Err, first, last)
			}
		}
	})

	t.Run("inside InTx it reads the transaction", func(t *testing.T) {
		err := pgexec.InTx(ctx, func(ctx context.Context) error {
			if _, err := pgexec.Exec(ctx, `INSERT INTO pgexec_batch (k, v) VALUES ('d', 4)`); err != nil {
				return err
			}
			var vs, n []int
			reads := []*pgexec.Read{
				pgexec.Rows(&vs, ints, `SELECT v FROM pgexec_batch WHERE k = $1`, "d"),
				pgexec.Rows(&n, ints, `SELECT count(*)::int FROM pgexec_batch`),
			}
			pgexec.Batch(ctx, reads...)
			if reads[0].Err != nil || reads[1].Err != nil || len(vs) != 1 || vs[0] != 4 || n[0] != 4 {
				t.Errorf("the transaction's own row isn't read: %v %v %v %v", reads[0].Err, reads[1].Err, vs, n)
			}
			return errRollBack
		})
		if err != errRollBack {
			t.Fatal(err)
		}
		var n []int
		r := pgexec.Rows(&n, ints, `SELECT count(*)::int FROM pgexec_batch`)
		pgexec.Batch(ctx, r)
		if r.Err != nil || n[0] != 3 {
			t.Errorf("after the rollback: %v %v", r.Err, n)
		}
	})
}

type rollBack struct{}

func (rollBack) Error() string { return "roll back" }

var errRollBack error = rollBack{}
