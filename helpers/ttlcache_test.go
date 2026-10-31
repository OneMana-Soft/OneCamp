package helpers

import (
	"math"
	"strconv"
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

// A Set costs about the same whether the cache holds two thousand entries
// or sixty-four thousand. Until 23 Oct every Set past 1,024 unexpired
// entries scanned the whole map for expired ones, under the write lock.
func TestTTLCacheSetCostDoesNotGrowWithItsSize(t *testing.T) {
	small, big := setCost(2_000), setCost(64_000)
	t.Logf("a Set takes %v beside 2,000 entries, %v beside 64,000", small, big)
	if big > 8*small {
		t.Fatalf("a Set took %v beside 64,000 entries and %v beside 2,000; want about the same", big, small)
	}
}

// setCost is the time per Set of 2,000 new keys into a cache that already
// holds `live` unexpired entries: the fastest of five rounds, so a round
// the scheduler or the collector interrupted doesn't count.
func setCost(live int) time.Duration {
	c := NewTTLCache[int](time.Hour)
	for i := range live {
		c.Set("live-"+strconv.Itoa(i), i)
	}
	keys := make([]string, 2_000)
	for i := range keys {
		keys[i] = "new-" + strconv.Itoa(i)
	}
	best := time.Duration(math.MaxInt64)
	for range 5 {
		start := time.Now()
		for _, k := range keys {
			c.Set(k, 0)
		}
		best = min(best, time.Since(start))
		for _, k := range keys {
			c.Delete(k)
		}
	}
	return best / time.Duration(len(keys))
}

// Expired entries nobody reads again are still swept: by the time the map
// has doubled, the ones that expired before it began to grow are gone.
func TestTTLCacheSweepsExpiredEntriesNobodyReads(t *testing.T) {
	c := NewTTLCache[int](time.Hour)
	const short = 3_000
	for i := range short {
		c.SetWithTTL("short-"+strconv.Itoa(i), i, time.Millisecond)
	}
	time.Sleep(2 * time.Millisecond)
	const long = 2*short + 1
	for i := range long {
		c.Set("long-"+strconv.Itoa(i), i)
	}
	if got := c.Len(); got != long {
		t.Fatalf("the cache holds %d entries, want only the %d unexpired ones", got, long)
	}
}
