package helpers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// EachBounded runs every item, never more than its limit at once, and returns
// only when all are done, a panicking one included.
func TestEachBoundedRunsEveryItemWithinItsLimit(t *testing.T) {
	var running, most, done atomic.Int32
	var mu sync.Mutex
	seen := map[int]bool{}
	items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	EachBounded("test", items, 3, func(n int) {
		now := running.Add(1)
		for {
			m := most.Load()
			if now <= m || most.CompareAndSwap(m, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		seen[n] = true
		mu.Unlock()
		running.Add(-1)
		done.Add(1)
		if n == 4 {
			panic("item 4 panics")
		}
	})
	if done.Load() != int32(len(items)) || len(seen) != len(items) {
		t.Fatalf("ran %d of %d items (%v)", done.Load(), len(items), seen)
	}
	if most.Load() > 3 || most.Load() < 2 {
		t.Fatalf("%d ran at once, want at most 3 (and more than one)", most.Load())
	}
	EachBounded("test", []int(nil), 0, func(int) { t.Fatal("ran an item of none") })
}
