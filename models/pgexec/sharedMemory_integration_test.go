//go:build integration

package pgexec_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A read outside a transaction that runs out of shared memory (SQLSTATE
// 53100) before it hands back a row runs once more without parallel workers
// (V-583). A parallel query's workers share their hashes in dynamic shared
// memory, a container's /dev/shm (64M unless compose sets shm_size), and
// several big reads at once run it out: on the L1 load test at 64M, 3,990
// window reads beside eight writers answered 503. A read without workers
// keeps its hashes in its own memory. A write is never run again.
func TestAReadThatRunsOutOfSharedMemoryRunsAgainWithoutWorkers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	// shm_full() runs out of shared memory while parallel workers may run,
	// and answers once they may not; shm_gone() runs out either way;
	// shm_write() writes a row first, as a function a SELECT calls may.
	const fullErr = `RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';`
	for _, s := range []string{
		`CREATE TABLE shm_t (k int NOT NULL)`,
		`CREATE FUNCTION shm_full() RETURNS text LANGUAGE plpgsql VOLATILE AS $$
		BEGIN
			IF current_setting('max_parallel_workers_per_gather') <> '0' THEN ` + fullErr + ` END IF;
			RETURN 'read without workers';
		END $$`,
		`CREATE FUNCTION shm_gone() RETURNS text LANGUAGE plpgsql VOLATILE AS $$ BEGIN ` + fullErr + ` END $$`,
		`CREATE FUNCTION shm_write() RETURNS text LANGUAGE plpgsql VOLATILE AS $$
		BEGIN
			INSERT INTO shm_t VALUES (9);
			RETURN shm_full();
		END $$`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	written := func() int {
		n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM shm_t`)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Each way of reading is answered by the run without workers.
	if s, err := pgexec.Scalar[string](ctx, `SELECT shm_full() AS k_scalar`); err != nil || s != "read without workers" {
		t.Errorf("Scalar: %q, %v; want it read again without workers", s, err)
	}
	if rows, err := pgexec.Query(ctx, `SELECT shm_full() AS k_rows FROM generate_series(1, 3)`, pgexec.Text); err != nil || len(rows) != 3 {
		t.Errorf("Query: %v, %v; want 3 rows read again without workers", rows, err)
	}
	if one, found, err := pgexec.QueryOne(ctx, `SELECT shm_full() AS k_one`, pgexec.Text); err != nil || !found || one != "read without workers" {
		t.Errorf("QueryOne: %q found %v, %v; want it read again without workers", one, found, err)
	}
	if pairs, err := pgexec.Pairs[int, string](ctx, `WITH g AS (SELECT generate_series(1, 2) AS g) SELECT g, shm_full() AS k_pairs FROM g`); err != nil ||
		len(pairs) != 2 || pairs[2] != "read without workers" {
		t.Errorf("Pairs (a WITH of reads): %v, %v; want 2 pairs read again without workers", pairs, err)
	}

	// The run without workers is a transaction of its own: the pool's
	// sessions keep their setting.
	for i := 0; i < 8; i++ {
		if w, err := pgexec.Scalar[string](ctx, `SELECT current_setting('max_parallel_workers_per_gather')`); err != nil || w == "0" {
			t.Fatalf("a session after a run again has max_parallel_workers_per_gather %q, %v; want the server's own", w, err)
		}
	}

	// Once more only: a read that runs out again is answered with it, saying
	// so, so its caller's log tells it from a read that wasn't run again.
	if _, err := pgexec.Scalar[string](ctx, `SELECT shm_gone() AS k_gone`); !helpers.IsDiskFull(err) ||
		!strings.Contains(fmt.Sprint(err), "again, without parallel workers") {
		t.Errorf("a read out of shared memory twice: %v; want its 53100, saying it ran out again", err)
	}

	// A read that has handed back rows isn't run again: its scan has seen
	// them, and would see them twice.
	scanned := 0
	_, err := pgexec.Query(ctx, `SELECT CASE WHEN g < 3 THEN 'row' ELSE shm_gone() END AS k_after FROM generate_series(1, 5) g`,
		func(r pgexec.Row) (string, error) {
			scanned++
			return pgexec.Text(r)
		})
	if !helpers.IsDiskFull(err) || scanned != 2 || !strings.Contains(fmt.Sprint(err), "after handing back rows") {
		t.Errorf("a read out of shared memory after 2 rows: %v after %d scanned; want its 53100, 2 scanned, not run again, saying why", err, scanned)
	}

	// In a transaction the error is its owner's: the transaction is aborted,
	// and only its owner can run it again (run again, shm_full would answer).
	err = pgexec.InTx(ctx, func(ctx context.Context) error {
		_, err := pgexec.Scalar[string](ctx, `SELECT shm_full() AS k_tx`)
		return err
	})
	if !helpers.IsDiskFull(err) {
		t.Errorf("in a transaction: %v; want its 53100, not run again", err)
	}

	// A write is never run again: not by Exec, not one that returns rows,
	// not a data-modifying WITH, and not one a SELECT's function makes (the
	// run again is READ ONLY, so Postgres refuses it). Each answers its own
	// 53100: run again, it would have answered or been refused READ ONLY.
	writes := []struct {
		kind string
		run  func(q string) error
		q    string
	}{
		{"k_exec", func(q string) error { _, err := pgexec.Exec(ctx, q); return err },
			`INSERT INTO shm_t SELECT 1 WHERE shm_full() IS NOT NULL /* k_exec */`},
		{"k_returning", func(q string) error { _, err := pgexec.Query(ctx, q, pgexec.Text); return err },
			`INSERT INTO shm_t SELECT 2 WHERE shm_full() IS NOT NULL RETURNING 'k_returning'`},
		{"k_with", func(q string) error { _, err := pgexec.Query(ctx, q, pgexec.Text); return err },
			`WITH w AS (INSERT INTO shm_t VALUES (3) RETURNING k) SELECT shm_full() AS k_with FROM w`},
	}
	for _, w := range writes {
		if err := w.run(w.q); !helpers.IsDiskFull(err) {
			t.Errorf("a write out of shared memory (%s): %v; want its 53100, not run again", w.kind, err)
		}
	}
	if _, err := pgexec.Scalar[string](ctx, `SELECT shm_write() AS k_hidden`); err == nil {
		t.Errorf("a SELECT whose function writes was answered after running out; want it refused, never written twice")
	}
	if n := written(); n != 0 {
		t.Errorf("%d rows written by writes that ran out of shared memory; want none", n)
	}
}

// WithoutWorkers holds for a statement whose plan a session cached while it
// had workers. pgx prepares each statement once per pooled session, and
// Postgres keeps a prepared statement's generic plan, Gather included, when a
// planner setting changes: a read run again without workers in a session
// whose plan for it has a Gather ran its workers, and ran out of shared
// memory again (V-583: the one window read of 3,990 that still answered 503
// at 64M, its run again taken by another of the pool's sessions). So for a
// statement with no parameter, whose cached generic plan Postgres runs
// whatever plan_cache_mode says (CR-226 F1), and for one with parameters
// once WithoutWorkers set no plan_cache_mode (V-611).
func TestAPlanCachedWithWorkersRunsWithoutThemAfterWithoutWorkers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	// One session, so every statement runs where its plan is cached. It
	// plans a statement once for all its runs (generic), with a Gather on top
	// that hands the whole plan to a worker (force_parallel_mode).
	postgresInit.DBConn.SqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_, _ = pgexec.Exec(ctx, `RESET ALL`)
		postgresInit.DBConn.SqlDB.SetMaxOpenConns(30)
	})
	leader, err := pgexec.Scalar[int](ctx, `SELECT pg_backend_pid()`)
	if err != nil {
		t.Fatal(err)
	}
	// shm_where(leader) says where it ran, and runs out of shared memory in
	// a worker while shm.out is on: a plan with workers in a small /dev/shm.
	for _, s := range []string{
		`SET force_parallel_mode = on`,
		`SET plan_cache_mode = force_generic_plan`,
		`SET shm.out = off`,
		`CREATE FUNCTION shm_where(leader int) RETURNS text LANGUAGE plpgsql VOLATILE PARALLEL SAFE AS $$
		BEGIN
			IF pg_backend_pid() = leader THEN RETURN 'leader'; END IF;
			IF current_setting('shm.out') = 'on' THEN
				RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
			END IF;
			RETURN 'worker';
		END $$`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, c := range []struct {
		name, read string
		args       []any
	}{
		{"with a parameter", `SELECT shm_where($1) AS k_cached FROM generate_series(1, 3)`, []any{leader}},
		{"with none", fmt.Sprintf(`SELECT shm_where(%d) AS k_cached_none FROM generate_series(1, 3)`, leader), nil},
	} {
		t.Run(c.name, func(t *testing.T) { cachedWithWorkers(ctx, t, c.read, c.args...) })
	}
}

// cachedWithWorkers is TestAPlanCachedWithWorkersRunsWithoutThemAfterWithoutWorkers
// for one read, on the test's one session, shm_where made and shm.out off.
func cachedWithWorkers(ctx context.Context, t *testing.T, read string, args ...any) {
	if _, err := pgexec.Exec(ctx, `SET shm.out = off`); err != nil {
		t.Fatal(err)
	}
	ran := func(ctx context.Context) ([]string, error) { return pgexec.Query(ctx, read, pgexec.Text, args...) }
	inTx := func(settings ...string) ([]string, error) {
		var where []string
		err := pgexec.InTx(ctx, func(ctx context.Context) (err error) {
			for _, s := range settings {
				if _, err := pgexec.Exec(ctx, s); err != nil {
					return err
				}
			}
			where, err = ran(ctx)
			return err
		})
		return where, err
	}

	// The session caches the read's plan with its worker.
	if where, err := ran(ctx); err != nil || len(where) != 3 || where[0] != "worker" {
		t.Fatalf("the read with workers ran in %v, %v; want 3 rows from a worker (no worker, no cached plan to test)", where, err)
	}
	if _, err := pgexec.Exec(ctx, `SET shm.out = on`); err != nil {
		t.Fatal(err)
	}

	// After WithoutWorkers the cached read runs in the session itself, as a
	// read's run again, a walk, a budgeted read and a ledger write rely on.
	if where, err := inTx(pgexec.WithoutWorkers); err != nil || len(where) != 3 || where[0] != "leader" {
		t.Errorf("after WithoutWorkers the cached read: %v, %v; want 3 rows read in the session, without workers", where, err)
	}

	// The settings are the transaction's: the session keeps its own.
	for _, s := range []string{"max_parallel_workers_per_gather", "plan_cache_mode"} {
		if v, err := pgexec.Scalar[string](ctx, `SELECT current_setting($1)`, s); err != nil || v == "0" || v == "force_custom_plan" {
			t.Errorf("after the transaction the session's %s is %q, %v; want its own", s, v, err)
		}
	}

	// The trap bites: without WithoutWorkers the cached plan's worker runs
	// out (in a transaction, so nothing runs it again).
	if where, err := inTx(); !helpers.IsDiskFull(err) {
		t.Errorf("the cached read with workers: %v, %v; want its worker's 53100", where, err)
	}

	// Outside a transaction a read that runs out this way is answered by its
	// run again, without workers.
	if where, err := ran(ctx); err != nil || len(where) != 3 || where[0] != "leader" {
		t.Errorf("a read out of shared memory in a worker: %v, %v; want 3 rows read again in the session", where, err)
	}
}

// A read a ledger's observer makes in the ledger's transaction, after the
// transaction ran WithoutWorkers, runs without workers, its arguments
// included (V-611). An observer (business/RowQuery's indexChanges,
// business/DataTable's graphInLedger) is handed the *sql.Tx and puts it in a
// ctx of its own (WithTx), so the transaction, not the ctx WithoutWorkers ran
// in, says so. WithoutWorkers sets no plan_cache_mode: re-planning a writer's
// every statement cost the ledger 31% of its writes (418 a second where 604
// without it, 100,000 rows at 64M), and without it a read whose session cached
// its plan with a Gather ran its workers there, and ran out of shared memory
// (53100) in a write that is never run again.
func TestAReadInATransactionThatRanWithoutWorkersRunsWithoutThem(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	// One session, which plans a statement once for all its runs (generic),
	// with a Gather on top that hands the whole plan to a worker.
	postgresInit.DBConn.SqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_, _ = pgexec.Exec(ctx, `RESET ALL`)
		postgresInit.DBConn.SqlDB.SetMaxOpenConns(30)
	})
	leader, err := pgexec.Scalar[int](ctx, `SELECT pg_backend_pid()`)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`SET force_parallel_mode = on`,
		`SET plan_cache_mode = force_generic_plan`,
		`SET shm.out = off`,
		`CREATE FUNCTION shm_where(leader int) RETURNS text LANGUAGE plpgsql VOLATILE PARALLEL SAFE AS $$
		BEGIN
			IF pg_backend_pid() = leader THEN RETURN 'leader'; END IF;
			IF current_setting('shm.out') = 'on' THEN
				RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
			END IF;
			RETURN 'worker';
		END $$`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// Shaped like models/postgres/DataTable's RowIDsAfter, which reindexes a
	// field's every row in a write's transaction: ids after one, in id
	// order, up to n.
	idsAfter := func(ctx context.Context) ([]string, error) {
		return pgexec.Query(ctx, `SELECT shm_where($1) AS k_ids_after FROM generate_series(1, 9) AS r(id)
			WHERE r.id > $2 ORDER BY r.id LIMIT $3`, pgexec.Text, leader, 0, 3)
	}
	if where, err := idsAfter(ctx); err != nil || len(where) != 3 || where[0] != "worker" {
		t.Fatalf("the read with workers ran in %v, %v; want 3 rows from a worker (no worker, no cached plan to test)", where, err)
	}
	if _, err := pgexec.Exec(ctx, `SET shm.out = on`); err != nil {
		t.Fatal(err)
	}

	// As ledgerTx: WithoutWorkers in a ctx of its own, then the observer's
	// read in another.
	var where []string
	err = postgresInit.InTx(ctx, pgexec.TxTimeout, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := pgexec.Exec(pgexec.WithTx(ctx, tx), pgexec.WithoutWorkers); err != nil {
			return err
		}
		var err error
		where, err = idsAfter(pgexec.WithTx(ctx, tx))
		return err
	})
	if err != nil || len(where) != 3 || where[0] != "leader" {
		t.Errorf("the observer's read after WithoutWorkers: %v, %v; want 3 rows read in the session, without workers", where, err)
	}

	// The mark is that transaction's: the session's next, which runs no
	// WithoutWorkers, runs the plan the session cached, and its worker runs
	// out.
	err = postgresInit.InTx(ctx, pgexec.TxTimeout, func(ctx context.Context, tx *sql.Tx) error {
		where, err = idsAfter(pgexec.WithTx(ctx, tx))
		return err
	})
	if !helpers.IsDiskFull(err) {
		t.Errorf("the read in a transaction that ran no WithoutWorkers: %v, %v; want its cached plan's worker's 53100", where, err)
	}
}

// caughtWarnings keeps what the server logs while a test runs: each line with
// its attributes, as otel_logs gets them.
type caughtWarnings struct {
	mu   sync.Mutex
	logs []string
}

func (c *caughtWarnings) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.logs...)
}

func (c *caughtWarnings) Enabled(context.Context, slog.Level) bool { return true }

func (c *caughtWarnings) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.String()
		return true
	})
	c.logs = append(c.logs, line)
	return nil
}

func (c *caughtWarnings) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *caughtWarnings) WithGroup(string) slog.Handler      { return c }

// catchWarnings points the server's logger at a caughtWarnings until the test ends.
func catchWarnings(t *testing.T) *caughtWarnings {
	c := &caughtWarnings{}
	saved := helpers.Logger
	helpers.Logger = slog.New(c)
	t.Cleanup(func() { helpers.Logger = saved })
	return c
}
