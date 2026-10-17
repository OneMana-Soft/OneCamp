package helpers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// countWaits stands in for time.After: it records each wait and lets the loop
// go on at once, or never, when fire is false.
func countWaits(t *testing.T, fire bool) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	was := retryAfter
	retryAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		c := make(chan time.Time, 1)
		if fire {
			c <- time.Time{}
		}
		return c
	}
	t.Cleanup(func() { retryAfter = was })
	return &waits
}

// failing is a try that fails n times, then succeeds, counting its calls.
func failing(n int, calls *int) func(context.Context) error {
	return func(context.Context) error {
		*calls++
		if *calls <= n {
			return errors.New("not up yet")
		}
		return nil
	}
}

func TestRetryWaitsFirstThenDoublesWhileUnderTheCap(t *testing.T) {
	waits := countWaits(t, true)
	calls := 0
	retry(context.Background(), Retries{First: 30 * time.Second, Cap: 2 * time.Minute, Tries: 10}, failing(4, &calls))
	if calls != 5 {
		t.Fatalf("tried %d times, want 5: four failures, then the success it stops at", calls)
	}
	// 30s, then doubled after each failure while under the 2m cap: 1m, 2m,
	// and 2m stays 2m.
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}
	if !reflect.DeepEqual(*waits, want) {
		t.Fatalf("waited %v, want %v", *waits, want)
	}
}

func TestRetryWithNoCapDoublesEveryTime(t *testing.T) {
	waits := countWaits(t, true)
	calls := 0
	retry(context.Background(), Retries{First: time.Minute, Tries: 4}, failing(10, &calls))
	if calls != 4 {
		t.Fatalf("tried %d times, want the 4 it may", calls)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	if !reflect.DeepEqual(*waits, want) {
		t.Fatalf("waited %v, want %v", *waits, want)
	}
}

func TestRetryStopsWhenItsContextEnds(t *testing.T) {
	countWaits(t, false) // the wait never ends: only the context can
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	retry(ctx, Retries{First: time.Hour, Cap: time.Hour, Tries: 3}, failing(0, &calls))
	if calls != 0 {
		t.Fatalf("tried %d times after its context ended", calls)
	}
}

func TestRetryInBackgroundReturnsAtOnceAndTriesOnItsOwn(t *testing.T) {
	countWaits(t, true)
	tried := make(chan struct{})
	RetryInBackground(context.Background(), "test", Retries{First: time.Hour, Tries: 1}, func(context.Context) error {
		close(tried)
		return nil
	})
	<-tried
}
