package pgexec

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"runtime"
	"sync"
	"time"
	"weak"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/jackc/pgx/v5"
)

// WithoutWorkers is the statement after which a transaction's reads keep
// their hashes in their own memory rather than sharing them with parallel
// workers in /dev/shm: no workers for the transaction alone
// (max_parallel_workers_per_gather 0). Exec marks the transaction it runs
// WithoutWorkers in, and from then on every read pgexec makes in that
// transaction, in whatever ctx carries it (WithTx), is planned for its run
// (plannedForItsRun). pgx prepares a statement once per pooled session, and
// Postgres keeps a prepared statement's generic plan, Gather included, when a
// planner setting changes, so a read whose session cached its plan with
// workers would still run them (V-583: a window read's run again, on another
// of the pool's sessions, ran out again). One statement, so it runs the same
// over either protocol.
//
// It sets no plan_cache_mode (V-611): force_custom_plan re-planned every
// statement in the transaction, a ledger write's own included, and cost the
// ledger 31% of its writes at the same cap (418 a second where 604 without
// it, 100,000 rows at 64M). A statement that writes or locks rows is never
// planned with workers, so it keeps its session's plan.
//
// Run it with Exec, never raw on a *sql.Tx (a raw run marks nothing), and
// make a read in the transaction through pgexec, never raw: a raw read runs
// its session's plan (rawReads_test.go names the raw statements on a
// transaction in the ledger's packages).
const WithoutWorkers = "SELECT set_config('max_parallel_workers_per_gather', '0', true)"

// withoutWorkersTxs holds each transaction Exec ran WithoutWorkers in, so the
// mark goes with the *sql.Tx rather than one ctx: business/RowQuery's
// indexChanges and business/DataTable's graphInLedger are handed a ledger's
// tx and put it in a ctx of their own (CR-226). A transaction is held by a
// weak pointer and leaves when its owner drops it.
var withoutWorkersTxs sync.Map // weak.Pointer[sql.Tx] -> struct{}

// markWithoutWorkers records that tx ran WithoutWorkers.
func markWithoutWorkers(tx *sql.Tx) {
	k := weak.Make(tx)
	if _, had := withoutWorkersTxs.LoadOrStore(k, struct{}{}); !had {
		runtime.AddCleanup(tx, func(k weak.Pointer[sql.Tx]) { withoutWorkersTxs.Delete(k) }, k)
	}
}

// ranWithoutWorkers reports whether the transaction ctx carries ran
// WithoutWorkers.
func ranWithoutWorkers(ctx context.Context) bool {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	if !ok || tx == nil {
		return false
	}
	_, marked := withoutWorkersTxs.Load(weak.Make(tx))
	return marked
}

// plannedForItsRun is a read's arguments as pgexec passes them in ctx. In a
// transaction that ran WithoutWorkers a read takes pgx's
// QueryExecModeDescribeExec ahead of its arguments, so Postgres plans it for
// this run (its unnamed statement, with these arguments) under the
// transaction's settings, rather than running the plan its session prepared
// once, maybe with workers (V-583, CR-226 F1; V-611 for a read with
// arguments). Anywhere else a read keeps its session's plan.
func plannedForItsRun(ctx context.Context, args []any) []any {
	if !ranWithoutWorkers(ctx) {
		return args
	}
	return append([]any{pgx.QueryExecModeDescribeExec}, args...)
}

// againWithoutWorkers runs read, and runs it once more without parallel
// workers when Postgres ran out of shared memory for it (helpers.IsDiskFull,
// 53100) before it handed back a row (read says whether it did). A parallel
// query's workers share their hashes in dynamic shared memory, a container's
// /dev/shm (Docker gives 64M unless compose sets shm_size), and several big
// reads at once run it out, where a read without workers keeps its hashes in
// its own memory (V-583: window reads beside writers on 100,000 rows; V-525
// first, for models/postgres/DataTable's budgeted reads).
//
// Only a read runs again (readOnly), never a write, and in a READ ONLY
// transaction of its own, so a write a read's function would make is refused
// rather than made. In a transaction the error is the caller's: the
// transaction is aborted, and only its owner can run it again.
//
// A read out of shared memory that isn't answered says why, so its caller's
// log tells one that ran out again from one that had handed back rows. Every
// session names each refusal in the log by its caller, once a minute a kind
// (postgresInit's spaceTracer), so a read run again and refused again is said
// once.
func againWithoutWorkers(ctx context.Context, q string, read func(ctx context.Context) (handed bool, err error)) error {
	handed, err := read(ctx)
	if err == nil || inTx(ctx) || !helpers.IsDiskFull(err) || !readOnly(q) {
		return err
	}
	if handed {
		return fmt.Errorf("pgexec: ran out of shared memory after handing back rows, so not read again: %w", err)
	}
	err = InTxFor(ctx, statementTimeout(), func(ctx context.Context) error {
		for _, s := range []string{"SET TRANSACTION READ ONLY", WithoutWorkers} {
			if _, err := Exec(ctx, s); err != nil {
				return err
			}
		}
		_, err := read(ctx)
		return err
	})
	if helpers.IsDiskFull(err) {
		return fmt.Errorf("pgexec: ran out of shared memory again, without parallel workers: %w", err)
	}
	return err
}

var (
	// startsReading is a statement that starts as a read: SELECT, WITH,
	// VALUES or TABLE, after any comments.
	startsReading = regexp.MustCompile(`(?is)^(\s+|--[^\n]*\n|/\*.*?\*/)*(SELECT|WITH|VALUES|TABLE)\b`)
	// writesWord is a word only a statement that writes or locks holds: a
	// data-modifying WITH's, a locking read's, or a sequence's. A name or a
	// literal holding one makes a read look like a write, which then isn't
	// run again: the safe side.
	writesWord = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE|COPY|CREATE|DROP|ALTER|TRUNCATE|LOCK|CALL|NOTIFY|NEXTVAL|SETVAL)\b|\bFOR\s+(KEY\s+)?SHARE\b`)
)

// readOnly is whether q only reads (a SELECT, with no data-modifying WITH and
// no locking clause), so it can run again. Pure.
func readOnly(q string) bool {
	return startsReading.MatchString(q) && !writesWord.MatchString(q)
}

// statementTimeout is a statement's budget outside a transaction: the pool's
// DBTimeout, 3 s when it has none.
func statementTimeout() time.Duration {
	if d := postgresInit.DBConn.DBTimeout; d > 0 {
		return d
	}
	return 3 * time.Second
}
