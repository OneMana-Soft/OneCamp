package helpers

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAllowsUpToTheLimitThenRefuses(t *testing.T) {
	l := NewRateLimiter(3, time.Minute)
	for i := 1; i <= 3; i++ {
		if !l.Allow("someone") {
			t.Fatalf("refused event %d of an allowance of 3", i)
		}
	}
	if l.Allow("someone") {
		t.Error("allowed a fourth event inside a window of three")
	}
}

func TestKeysAreCountedSeparately(t *testing.T) {
	l := NewRateLimiter(1, time.Minute)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("one caller's allowance was spent by another's")
	}
	if l.Allow("a") {
		t.Error("the second caller's event refilled the first caller's window")
	}
}

func TestTheWindowReopens(t *testing.T) {
	l := NewRateLimiter(1, 20*time.Millisecond)
	if !l.Allow("someone") {
		t.Fatal("refused the first event")
	}
	if l.Allow("someone") {
		t.Fatal("allowed a second event inside the window")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("someone") {
		t.Error("the window never reopened, so one burst locks a caller out for good")
	}
}

// A caller the handler could not identify must not be folded into one shared
// bucket with everybody else it also could not identify.
func TestAnUnidentifiedCallerIsNotLimited(t *testing.T) {
	l := NewRateLimiter(1, time.Minute)
	for i := 0; i < 5; i++ {
		if !l.Allow("") {
			t.Fatal("an empty key was rate limited, which limits every anonymous caller together")
		}
	}
}

func TestZeroLimitAllowsEverything(t *testing.T) {
	l := NewRateLimiter(0, time.Minute)
	for i := 0; i < 10; i++ {
		if !l.Allow("someone") {
			t.Fatal("a limit of zero must mean off, not closed")
		}
	}
}

// The limiter this replaces kept an entry per caller for the life of the
// process. On a public endpoint that is an unbounded map that nothing ever
// touches again.
func TestExpiredKeysAreEvicted(t *testing.T) {
	l := NewRateLimiter(1, 10*time.Millisecond)
	for i := 0; i < 500; i++ {
		l.Allow(fmt.Sprintf("caller-%d", i))
	}
	time.Sleep(20 * time.Millisecond)
	// Any call is enough to trigger the amortised pass.
	l.Allow("someone-new")

	l.mu.Lock()
	held := len(l.seen)
	l.mu.Unlock()
	if held > 1 {
		t.Errorf("kept %d expired windows; the map only ever grows", held)
	}
}

func TestRetrySaysWhenToComeBack(t *testing.T) {
	l := NewRateLimiter(1, time.Minute)
	if d := l.Retry("nobody"); d != 0 {
		t.Errorf("a caller with no window should wait %v, not %v", 0, d)
	}
	l.Allow("someone")
	if d := l.Retry("someone"); d <= 0 || d > time.Minute {
		t.Errorf("wanted a wait inside the window, got %v", d)
	}
}

func TestConcurrentCallersDoNotRace(t *testing.T) {
	l := NewRateLimiter(100, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Allow(fmt.Sprintf("caller-%d", n%5))
		}(i)
	}
	wg.Wait()
}

// The window has to reopen on its own, not as a side effect of the eviction
// pass happening to run first.
//
// Eviction runs at most once per window, so a key can expire during a cycle the
// sweep has already made. In that gap the only thing that lets the caller back
// in is the expiry check on the lookup itself. Without this case, deleting that
// check leaves every other test passing, because the sweep collects the expired
// key a moment before it is looked up.
func TestTheWindowReopensBeforeTheNextSweep(t *testing.T) {
	const window = 100 * time.Millisecond
	l := NewRateLimiter(1, window)

	l.Allow("first")            // starts the sweep clock: next pass due at +100ms
	time.Sleep(window * 6 / 10) // +60ms
	if !l.Allow("subject") {    // expires at +160ms
		t.Fatal("refused a key's first event")
	}

	time.Sleep(window / 2) // +110ms: the sweep is due, runs, and is pushed out to +210ms
	l.Allow("bystander")

	time.Sleep(window * 6 / 10) // +170ms: subject has expired, the next sweep is 40ms away
	if !l.Allow("subject") {
		t.Error("a caller stayed locked out after their own window ended, " +
			"until an eviction pass they have no way to trigger")
	}
}
