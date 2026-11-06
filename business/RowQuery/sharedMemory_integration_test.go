//go:build integration

package business_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// A budgeted read that runs out of shared memory is read again without
// parallel workers, within what is left of its budget (V-525). A parallel hash
// join keeps its table in dynamic shared memory (a container's /dev/shm), so
// three big summaries reads at once ran it out (SQLSTATE 53100) and lost their
// sums; a read without workers keeps its hashes in its own memory.
func TestABudgetedReadThatRunsOutOfSharedMemoryIsReadAgainWithoutWorkers(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	shmFull := &pgconn.PgError{Severity: "ERROR", Code: "53100",
		Message: `could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device`}
	// One row over no rows: the set's count, and the settings the read ran under.
	settings := `COUNT(*), current_setting('max_parallel_workers_per_gather'), current_setting('statement_timeout')`
	type attempt struct{ workers, timeout string }
	read := func(budget time.Duration, fail func(n int) error) ([]attempt, error) {
		var seen []attempt
		err := model.SummarizeRowsWhere(ctx, uuid.New(), "", settings, "TRUE", "", nil, budget, func(r pgexec.Row) error {
			var a attempt
			var rows int64
			if err := r.Scan(&rows, &a.workers, &a.timeout); err != nil {
				return err
			}
			seen = append(seen, a)
			return fail(len(seen))
		})
		return seen, err
	}

	// Out of shared memory once: read again with no workers, under the budget.
	seen, err := read(2*time.Second, func(n int) error {
		if n == 1 {
			return shmFull
		}
		return nil
	})
	if err != nil || len(seen) != 2 {
		t.Fatalf("a read out of shared memory: %v after %d reads (%v); want it read again, and answered", err, len(seen), seen)
	}
	if seen[0].workers == "0" || seen[1].workers != "0" {
		t.Errorf("workers per gather %q then %q; want the server's own first, then 0", seen[0].workers, seen[1].workers)
	}
	if left, err := time.ParseDuration(seen[1].timeout); err != nil || left <= 0 || left > 2*time.Second {
		t.Errorf("the second read's statement_timeout %q (%v); want what is left of the 2 s budget", seen[1].timeout, err)
	}

	// Out of shared memory with the budget spent: past its budget, never read again.
	seen, err = read(100*time.Millisecond, func(int) error {
		time.Sleep(150 * time.Millisecond)
		return shmFull
	})
	if !errors.Is(err, model.ErrSummariesTooLong) || len(seen) != 1 {
		t.Errorf("out of shared memory past the budget: %v after %d reads; want ErrSummariesTooLong after one", err, len(seen))
	}

	// Out of memory twice: the second read's error, never a third read.
	seen, err = read(2*time.Second, func(int) error { return shmFull })
	if err == nil || errors.Is(err, model.ErrSummariesTooLong) || len(seen) != 2 {
		t.Errorf("out of shared memory twice: %v after %d reads; want the error after two", err, len(seen))
	}

	// Any other failure is the read's own: never read again.
	other := &pgconn.PgError{Severity: "ERROR", Code: "53200", Message: "out of memory"}
	seen, err = read(2*time.Second, func(int) error { return other })
	if !errors.As(err, new(*pgconn.PgError)) || len(seen) != 1 {
		t.Errorf("out of memory (53200): %v after %d reads; want that error after one", err, len(seen))
	}
}
