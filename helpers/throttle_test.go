package helpers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of requests for one key is two runs: the first at once, and one
// when its window closes, so the last change in the burst is never lost.
func TestAThrottledBurstRunsFirstAndOnceMoreAtTheEnd(t *testing.T) {
	th := NewThrottle("test", 60*time.Millisecond)
	var runs atomic.Int32
	run := func() { runs.Add(1) }
	for i := 0; i < 100; i++ {
		th.Do("call-1", run)
	}
	if !th.Settle(2 * time.Second) {
		t.Fatal("the throttle never settled")
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("a burst of 100 ran %d times, want 2 (first, and once at the window's end)", got)
	}
}

// One request is one run, and nothing is kept for the key afterwards.
func TestOneThrottledRequestRunsOnceAndLeavesNothingBehind(t *testing.T) {
	th := NewThrottle("test", 30*time.Millisecond)
	var runs atomic.Int32
	th.Do("call-1", func() { runs.Add(1) })
	if !th.Settle(2 * time.Second) {
		t.Fatal("the throttle never settled")
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("one request ran %d times, want 1", got)
	}
	th.mu.Lock()
	left := len(th.due)
	th.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d keys are still held after their window closed", left)
	}
}

// Keys don't hold each other up: each has its own window.
func TestThrottledKeysRunIndependently(t *testing.T) {
	th := NewThrottle("test", 200*time.Millisecond)
	var mu sync.Mutex
	ran := map[string]time.Time{}
	start := time.Now()
	for _, k := range []string{"a", "b", "c"} {
		k := k
		th.Do(k, func() {
			mu.Lock()
			ran[k] = time.Now()
			mu.Unlock()
		})
	}
	if !th.Settle(2 * time.Second) {
		t.Fatal("the throttle never settled")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{"a", "b", "c"} {
		at, ok := ran[k]
		if !ok {
			t.Fatalf("key %s never ran", k)
		}
		if at.Sub(start) > 150*time.Millisecond {
			t.Errorf("key %s waited %v for another key's window", k, at.Sub(start))
		}
	}
}
