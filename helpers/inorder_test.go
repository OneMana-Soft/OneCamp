package helpers

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One key's pieces run one at a time, in the order they were handed over,
// though the first holds every later one up; another key's run alongside; a
// piece that panics lets the next one go; and a key is forgotten once its
// pieces are done.
func TestInOrderRunsAKeysWorkInTheOrderItWasHandedOver(t *testing.T) {
	var (
		o          InOrder
		mu         sync.Mutex
		ran        []int
		running    atomic.Int32
		overlapped atomic.Bool
		letGo      = make(chan struct{})
	)
	for i := range 50 {
		o.Go("a", "test.in-order", func() {
			if running.Add(1) > 1 {
				overlapped.Store(true)
			}
			defer running.Add(-1)
			if i == 0 {
				<-letGo
			}
			if i == 7 {
				panic("a piece that breaks")
			}
			mu.Lock()
			ran = append(ran, i)
			mu.Unlock()
		})
	}

	// Another key's piece doesn't wait for "a"'s first, which is held.
	other := make(chan struct{})
	o.Go("b", "test.in-order", func() { close(other) })
	select {
	case <-other:
	case <-time.After(time.Minute):
		t.Fatal("a piece for another key waited for the held one")
	}

	close(letGo)
	if !goSafeWork.Settle(time.Minute) {
		t.Fatal("the pieces did not finish")
	}
	var want []int
	for i := range 50 {
		if i != 7 {
			want = append(want, i)
		}
	}
	if !slices.Equal(ran, want) {
		t.Errorf("ran %v, want every piece but the one that panicked, in the order handed over", ran)
	}
	if overlapped.Load() {
		t.Error("two of one key's pieces ran at once")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.last) != 0 {
		t.Errorf("keys kept after their pieces were done: %v", o.last)
	}
}
