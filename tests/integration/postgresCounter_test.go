//go:build integration
// +build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// A counted call's statements are the count, whatever else the process sends
// on the pool meanwhile: v2.75.4's public CI counted work an earlier test left
// going, and a read looked as if it asked per task (8 for 600 tasks, 5 for 60).
func TestCountPostgresCountsOnlyTheMeasuredCall(t *testing.T) {
	env := SetupEnv(t)
	counter := CountPostgres(t, env)

	// Someone else's work on the same pool: an earlier test's, still going.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = postgresInit.DBConn.SqlDB.ExecContext(context.Background(), `SELECT pg_sleep(0.001)`)
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
	time.Sleep(50 * time.Millisecond)

	counter.Reset()
	ctx := counter.Context(context.Background())
	var n int
	for i := 0; i < 3; i++ {
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err := postgresInit.DBConn.GormDB.WithContext(ctx).Exec(`SELECT 2`).Error; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := counter.Count(); got != 4 {
		t.Fatalf("the measured call sent 4 statements (3 through SQL, 1 through gorm); counted %d", got)
	}
}

// A failed test is told it never connected the server's pool only when it
// didn't: one that did was reached by a stale handle, work an earlier test
// left going.
func TestNeverConnectedIsSaidOnlyToATestThatNeverConnected(t *testing.T) {
	if neverConnectedHint(false, 2, 3) == "" {
		t.Error("a test that never connected and reached the empty pool is not told so")
	}
	if got := neverConnectedHint(true, 2, 3); got != "" {
		t.Errorf("a test that connected the pool is told %q", got)
	}
	if got := neverConnectedHint(false, 3, 3); got != "" {
		t.Errorf("a test that never reached the empty pool is told %q", got)
	}
}
