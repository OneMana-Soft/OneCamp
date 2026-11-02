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
// V-147 (CR-123): a count that fails is tried once more, in the same run, so
// one that fails is tried twice in all, never more.
func TestAFailingCountNeverFailsTheFill(t *testing.T) {
	saved := vacuumRows
	t.Cleanup(func() { vacuumRows = saved })
	calls := 0
	vacuumRows = func(ctx context.Context) error {
		calls++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("canceling statement due to lock timeout")
	}
	if err := CountAfterFill(context.Background(), analyzeAfter); err != nil {
		t.Errorf("a count that failed failed the fill: %v", err)
	}
	if calls != 2 {
		t.Errorf("%d rows filled, and a count that kept failing was tried %d times, where it's tried once and once more", analyzeAfter, calls)
	}
	if err := CountAfterFill(context.Background(), analyzeAfter-1); err != nil || calls != 2 {
		t.Errorf("%d rows filled: counted (%d calls, error %v), where so few aren't", analyzeAfter-1, calls, err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CountAfterFill(stopped, analyzeAfter); !errors.Is(err, context.Canceled) {
		t.Errorf("a stopped fill's count: %v, where it hears that it was stopped", err)
	}
}

// V-147 (CR-123): a count that fails once (a lock held past its time on a
// loaded machine) is tried again at once, and the planner counts the fill
// then; a count that passes isn't tried again.
func TestACountThatFailsOnceIsTriedAgain(t *testing.T) {
	saved := vacuumRows
	t.Cleanup(func() { vacuumRows = saved })
	calls, failFirst := 0, true
	vacuumRows = func(context.Context) error {
		calls++
		if failFirst && calls == 1 {
			return errors.New("canceling statement due to statement timeout")
		}
		return nil
	}
	if err := CountAfterFill(context.Background(), analyzeAfter); err != nil || calls != 2 {
		t.Errorf("a count that failed once: %d tries (error %v), where the second counts the fill", calls, err)
	}
	calls, failFirst = 0, false
	if err := CountAfterFill(context.Background(), analyzeAfter); err != nil || calls != 1 {
		t.Errorf("a count that passed: %d tries (error %v), where one is all", calls, err)
	}
}
