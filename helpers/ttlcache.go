package helpers

import (
	"sync"
	"time"
)

// TTLCache is a small, dependency-free, generic in-memory cache with
// per-entry expiry. It is the canonical replacement for the various
// ad-hoc map[string]*entry caches we used to maintain inside
// individual business packages (GitHub OAuth bot login, GitHub user
// mapping, webhook fan-out lists, etc.).
//
// Design notes
// ------------
//   - The cache is intentionally simple. It does not track LRU order
//     and does not run a background sweeper. Entries are evicted lazily
//     on access, plus a Set sweeps out expired entries once the map has
//     doubled since the last sweep (and holds over minSweepAt). A sweep
//     scans the whole map, so it waits for as many inserts as it scans:
//     a Set costs O(1) amortised, and expired entries nobody reads again
//     never make the map more than twice what was live at the last
//     sweep. This keeps the cache predictable and avoids leaking
//     goroutines.
//   - Negative results (e.g. "no link for this repo", "GitHub user has
//     no OneCamp mapping") should be cached too — typically with a
//     shorter TTL via SetWithTTL — to avoid hammering the DB on
//     repeated misses. Use the zero value of V to signal a negative.
//   - Concurrency: backed by a single sync.RWMutex. For the read-heavy
//     workloads we have (one writer per minute, thousands of readers
//     per second on hot caches) this is fine. If a future caller
//     needs sharded buckets, swap the implementation here without
//     changing the surface.
type TTLCache[V any] struct {
	mu         sync.RWMutex
	items      map[string]ttlEntry[V]
	defaultTTL time.Duration
	// sweepAt is the size past which the next Set sweeps.
	sweepAt int
	// maxEntries bounds the cache (NewBoundedTTLCache); 0 is unbounded.
	maxEntries int
	// now is the clock entries age by: time.Now unless NewTTLCacheClocked
	// was given one.
	now func() time.Time
}

// minSweepAt keeps small caches from sweeping every few inserts.
const minSweepAt = 1024

type ttlEntry[V any] struct {
	value     V
	expiresAt time.Time
}

// NewTTLCache returns a cache with the given default TTL. Values stored
// via Set use this TTL; SetWithTTL overrides on a per-entry basis.
//
// A defaultTTL <= 0 disables time-based expiry — entries live forever
// until Delete or Purge is called. Use Forever() instead of passing 0
// directly to make intent obvious at the call site.
func NewTTLCache[V any](defaultTTL time.Duration) *TTLCache[V] {
	return &TTLCache[V]{
		items:      make(map[string]ttlEntry[V]),
		defaultTTL: defaultTTL,
		sweepAt:    minSweepAt,
	}
}

// NewTTLCacheClocked is NewTTLCache with entries aged by now instead of
// time.Now: for a cache whose package holds its clock still in a test.
func NewTTLCacheClocked[V any](defaultTTL time.Duration, now func() time.Time) *TTLCache[V] {
	c := NewTTLCache[V](defaultTTL)
	c.now = now
	return c
}

func (c *TTLCache[V]) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Get returns the cached value and true when an unexpired entry exists.
// Expired entries are deleted opportunistically on access so a slow-
// burn miss does not accumulate dead memory.
func (c *TTLCache[V]) Get(key string) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return zero, false
	}
	if !entry.expiresAt.IsZero() && c.clock().After(entry.expiresAt) {
		c.mu.Lock()
		// Re-check under the write lock so a concurrent Set isn't
		// clobbered.
		if cur, stillThere := c.items[key]; stillThere && cur.expiresAt == entry.expiresAt {
			delete(c.items, key)
		}
		c.mu.Unlock()
		return zero, false
	}
	return entry.value, true
}

// Set stores the value under key with the cache's default TTL.
// NewBoundedTTLCache is NewTTLCache holding at most maxEntries, for a cache
// whose keys come from requests. A Set of a new key at the bound sweeps the
// expired entries and, while fewer than an eighth of the bound are free,
// drops arbitrary ones: a cache full of live entries pays one scan per
// maxEntries/8 inserts, never one per insert.
func NewBoundedTTLCache[V any](defaultTTL time.Duration, maxEntries int) *TTLCache[V] {
	c := NewTTLCache[V](defaultTTL)
	c.maxEntries = maxEntries
	return c
}

func (c *TTLCache[V]) Set(key string, value V) {
	if c == nil {
		return
	}
	c.SetWithTTL(key, value, c.defaultTTL)
}

// SetWithTTL stores the value with a caller-provided TTL. A ttl <= 0
// stores the entry without expiry (purge-only). This is the right
// primitive for negative caching with a shorter TTL than positive
// hits.
func (c *TTLCache[V]) SetWithTTL(key string, value V, ttl time.Duration) {
	if c == nil {
		return
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.clock().Add(ttl)
	}
	c.mu.Lock()
	if _, held := c.items[key]; !held && c.maxEntries > 0 && len(c.items) >= c.maxEntries {
		c.makeRoomLocked()
	}
	c.items[key] = ttlEntry[V]{value: value, expiresAt: expiresAt}
	if len(c.items) > c.sweepAt {
		c.sweepLocked()
	}
	c.mu.Unlock()
}

// makeRoomLocked frees at least an eighth of a bounded cache: the expired
// entries, then arbitrary ones. Must be called with the write lock held.
func (c *TTLCache[V]) makeRoomLocked() {
	c.sweepLocked()
	keep := c.maxEntries - max(1, c.maxEntries/8)
	for k := range c.items {
		if len(c.items) <= keep {
			break
		}
		delete(c.items, k)
	}
}

// Delete drops a single key. Safe to call on missing keys.
func (c *TTLCache[V]) Delete(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

// sweepLocked removes expired entries and sets the next sweep for when
// the map has doubled. Must be called with the write lock held.
func (c *TTLCache[V]) sweepLocked() {
	now := c.clock()
	for k, v := range c.items {
		if !v.expiresAt.IsZero() && now.After(v.expiresAt) {
			delete(c.items, k)
		}
	}
	c.sweepAt = max(minSweepAt, 2*len(c.items))
}

// Purge drops every entry. Used on disconnect / re-connect flows
// where the entire cache scope is invalidated at once.
func (c *TTLCache[V]) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = make(map[string]ttlEntry[V])
	c.sweepAt = minSweepAt
	c.mu.Unlock()
}

// Len returns the current entry count, including any that may have
// expired but not yet been swept. Useful for metrics and tests.
func (c *TTLCache[V]) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
