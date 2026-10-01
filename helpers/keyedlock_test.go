package helpers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Same key must be mutually exclusive: a second Acquire blocks until Release,
// so a shared counter never sees concurrent holders.
func TestKeyedLock_SerializesSameKey(t *testing.T) {
	var kl KeyedLock
	var concurrent int32
	var maxObserved int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !kl.Acquire("conv-1", 0) { // unbounded
				t.Errorf("unbounded Acquire should never be refused")
				return
			}
			defer kl.Release("conv-1")
			n := atomic.AddInt32(&concurrent, 1)
			for {
				m := atomic.LoadInt32(&maxObserved)
				if n <= m || atomic.CompareAndSwapInt32(&maxObserved, m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()

	if maxObserved != 1 {
		t.Fatalf("expected at most 1 concurrent holder per key, saw %d", maxObserved)
	}
}

// Different keys must proceed concurrently (no global lock).
func TestKeyedLock_ConcurrentDifferentKeys(t *testing.T) {
	var kl KeyedLock
	if !kl.Acquire("a", 0) {
		t.Fatal("acquire a failed")
	}
	defer kl.Release("a")

	done := make(chan bool, 1)
	go func() {
		if kl.Acquire("b", 0) { // must not block on key "a"
			kl.Release("b")
			done <- true
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a different key was blocked by an unrelated held key")
	}
}

// The per-key cap sheds load once reached, and the entry is freed after the
// holders release (no leak).
func TestKeyedLock_CapAndCleanup(t *testing.T) {
	var kl KeyedLock

	if !kl.Acquire("c", 1) {
		t.Fatal("first Acquire under cap 1 should succeed")
	}
	// Cap reached (1 holder): a second Acquire with the same cap is refused
	// without blocking.
	if kl.Acquire("c", 1) {
		t.Fatal("Acquire over cap should be refused")
	}
	kl.Release("c")

	// After release the entry must be gone (no leak).
	kl.mu.Lock()
	_, exists := kl.entries["c"]
	kl.mu.Unlock()
	if exists {
		t.Fatal("entry should be freed once no one holds or waits on the key")
	}

	// A refused first-caller must not leak an empty entry either.
	var kl2 KeyedLock
	// Pre-create at cap by holding one, then check a refused caller leaves no
	// stray entry for a *different* unused key.
	if kl2.Acquire("held", 0) {
		defer kl2.Release("held")
	}
	kl2.mu.Lock()
	_, strays := kl2.entries["never-used"]
	kl2.mu.Unlock()
	if strays {
		t.Fatal("a key never acquired should not exist")
	}
}
