package business

import (
	"context"
	"errors"
	"testing"
)

// V-107 (CR-96 M1): a fill whose planner count fails still stands. The
// count's failure (a concurrent ANALYZE held it past its timeout) leaves the
// planner to the autovacuum; a whole index marked stale for it would have
// the sweep walk the table again. Only a stopped fill hears the error.
func TestAFailingCountNeverFailsTheFill(t *testing.T) {
	saved := analyzeRows
	t.Cleanup(func() { analyzeRows = saved })
	calls := 0
	analyzeRows = func(ctx context.Context) error {
		calls++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("canceling statement due to lock timeout")
	}
	if err := CountAfterFill(context.Background(), analyzeAfter); err != nil {
		t.Errorf("a count that failed failed the fill: %v", err)
	}
	if calls != 1 {
		t.Errorf("%d rows filled were counted %d times, where once is", analyzeAfter, calls)
	}
	if err := CountAfterFill(context.Background(), analyzeAfter-1); err != nil || calls != 1 {
		t.Errorf("%d rows filled: counted (%d calls, error %v), where so few aren't", analyzeAfter-1, calls, err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CountAfterFill(stopped, analyzeAfter); !errors.Is(err, context.Canceled) {
		t.Errorf("a stopped fill's count: %v, where it hears that it was stopped", err)
	}
}
