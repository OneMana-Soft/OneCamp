package pgexec

import (
	"context"
	"database/sql"
	"runtime"
	"slices"
	"testing"
	"time"
	"weak"

	"github.com/jackc/pgx/v5"
)

// Only a statement that only reads runs again without workers (V-583): a
// SELECT, a WITH of reads, VALUES or TABLE. Anything that writes or locks,
// or might, never does, a word in a name included.
func TestOnlyAReadRunsAgain(t *testing.T) {
	for q, want := range map[string]bool{
		`SELECT r.id FROM data_table_rows r WHERE r.table_id = $1 AND r.deleted_at IS NULL`: true,
		"\n\t\tselect created_at, updated_at from t":                                        true,
		`-- the window
		/* sorted */ SELECT 1`: true,
		`WITH g AS (SELECT 1) SELECT * FROM g`: true,
		`VALUES (1)`:                           true,
		`TABLE t`:                              true,
		`INSERT INTO t SELECT 1`:               false,
		`UPDATE t SET v = 1`:                   false,
		`DELETE FROM t`:                        false,
		`WITH w AS (INSERT INTO t VALUES (1) RETURNING k) SELECT * FROM w`: false,
		`WITH w AS (DELETE FROM t RETURNING k) SELECT * FROM w`:            false,
		`SELECT * FROM t FOR UPDATE`:                                       false,
		`SELECT * FROM t FOR NO KEY UPDATE`:                                false,
		`SELECT * FROM t FOR SHARE`:                                        false,
		`SELECT * FROM t FOR KEY SHARE`:                                    false,
		`SELECT nextval('s')`:                                              false,
		`SELECT setval('s', 1)`:                                            false,
		`SELECT "lock" FROM t`:                                             false,
		`CREATE TABLE t AS SELECT 1`:                                       false,
		`EXPLAIN ANALYZE DELETE FROM t`:                                    false,
		``:                                                                 false,
	} {
		if got := readOnly(q); got != want {
			t.Errorf("readOnly(%q) = %v; want %v", q, got, want)
		}
	}
}

// A read in a transaction that ran WithoutWorkers is planned for its run,
// its arguments kept behind pgx's mode (CR-226 F1; V-611). The mark is the
// *sql.Tx's, so a ctx of its own sees it (an observer handed a ledger's tx);
// only WithoutWorkers makes it. In a transaction that ran none, or outside
// one, a read passes its arguments as they are.
func TestAReadInATransactionThatRanWithoutWorkersIsPlannedForItsRun(t *testing.T) {
	ctx := context.Background()
	pool := sql.OpenDB(&recorder{})
	defer pool.Close()
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	described := func(args ...any) []any { return append([]any{pgx.QueryExecModeDescribeExec}, args...) }

	if got := plannedForItsRun(WithTx(ctx, tx), []any{7}); !slices.Equal(got, []any{7}) {
		t.Errorf("before WithoutWorkers: %v; want the arguments as they were", got)
	}
	if _, err := Exec(WithTx(ctx, tx), "SET LOCAL work_mem = '64kB'"); err != nil {
		t.Fatal(err)
	}
	if got := plannedForItsRun(WithTx(ctx, tx), nil); len(got) != 0 {
		t.Errorf("after another setting: %v; want none (only WithoutWorkers marks)", got)
	}
	if _, err := Exec(WithTx(ctx, tx), WithoutWorkers); err != nil {
		t.Fatal(err)
	}
	observer := WithTx(context.Background(), tx)
	if got := plannedForItsRun(observer, []any{7, "x"}); !slices.Equal(got, described(7, "x")) {
		t.Errorf("arguments after WithoutWorkers, in a ctx of its own: %v; want QueryExecModeDescribeExec, then them", got)
	}
	if got := plannedForItsRun(observer, nil); !slices.Equal(got, described()) {
		t.Errorf("no argument after WithoutWorkers: %v; want QueryExecModeDescribeExec alone", got)
	}
	if got := plannedForItsRun(WithTx(ctx, &sql.Tx{}), []any{7}); !slices.Equal(got, []any{7}) {
		t.Errorf("another transaction: %v; want the arguments as they were", got)
	}
	if got := plannedForItsRun(ctx, nil); len(got) != 0 {
		t.Errorf("outside a transaction: %v; want none", got)
	}
}

// A transaction's mark leaves with it: once its owner drops it, the
// collector takes the mark away, so marks don't pile up with the writes.
func TestATransactionsMarkLeavesWithIt(t *testing.T) {
	k := func() weak.Pointer[sql.Tx] {
		tx := &sql.Tx{}
		markWithoutWorkers(tx)
		if !ranWithoutWorkers(WithTx(context.Background(), tx)) {
			t.Fatal("a transaction just marked isn't")
		}
		return weak.Make(tx)
	}()
	for range 100 {
		runtime.GC()
		if _, held := withoutWorkersTxs.Load(k); !held {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a dropped transaction's mark is still held after 100 collections")
}
