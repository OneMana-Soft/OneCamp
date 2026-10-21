package helpers

import (
	"sync"
	"testing"
	"time"
)

// Smoke test for the basic Get/Set/Delete contract.
func TestTTLCacheSetGetDelete(t *testing.T) {
	c := NewTTLCache[int](time.Minute)

	if _, ok := c.Get("missing"); ok {
		t.Fatalf("expected miss on empty cache")
	}

	c.Set("a", 1)
	if got, ok := c.Get("a"); !ok || got != 1 {
		t.Fatalf("expected hit value=1, got=%d ok=%v", got, ok)
	}

	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatalf("expected miss after delete")
	}
}

// SetWithTTL respects a per-entry override and the entry actually
// expires when the clock crosses the boundary.
func TestTTLCachePerEntryTTL(t *testing.T) {
	c := NewTTLCache[string](time.Hour)

	c.SetWithTTL("short", "v", 5*time.Millisecond)
	if _, ok := c.Get("short"); !ok {
		t.Fatalf("entry should be live just after Set")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("short"); ok {
		t.Fatalf("entry should have expired")
	}
}

// Purge wipes everything in one shot.
func TestTTLCachePurge(t *testing.T) {
	c := NewTTLCache[int](time.Minute)
	c.Set("a", 1)
	c.Set("b", 2)
	if c.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", c.Len())
	}
	c.Purge()
	if c.Len() != 0 {
		t.Fatalf("expected 0 entries after purge, got %d", c.Len())
	}
}

// Concurrent writers and readers should not race or corrupt the map.
// We rely on `go test -race` in CI to amplify any issues here.
func TestTTLCacheConcurrent(t *testing.T) {
	c := NewTTLCache[int](time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := string(rune('a' + (i+j)%26))
				c.Set(k, j)
				_, _ = c.Get(k)
			}
		}(i)
	}
	wg.Wait()
}

// nil receiver methods must be no-ops so callers don't have to guard.
func TestTTLCacheNilSafe(t *testing.T) {
	var c *TTLCache[int]
	c.Set("a", 1)
	if _, ok := c.Get("a"); ok {
		t.Fatalf("nil cache must miss")
	}
	c.Delete("a")
	c.Purge()
	if c.Len() != 0 {
		t.Fatalf("nil cache len must be 0")
	}
}
