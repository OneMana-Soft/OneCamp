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
//     on access, plus an opportunistic sweep happens whenever Set is
//     called and the map has grown past sweepThreshold. This keeps the
//     cache predictable and avoids leaking goroutines.
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
	mu             sync.RWMutex
	items          map[string]ttlEntry[V]
	defaultTTL     time.Duration
	sweepThreshold int
}

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
		items:          make(map[string]ttlEntry[V]),
		defaultTTL:     defaultTTL,
		sweepThreshold: 1024,
	}
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
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
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
		expiresAt = time.Now().Add(ttl)
	}
	c.mu.Lock()
	c.items[key] = ttlEntry[V]{value: value, expiresAt: expiresAt}
	if len(c.items) > c.sweepThreshold {
		c.sweepLocked()
	}
	c.mu.Unlock()
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

// sweepLocked removes expired entries. Must be called with the write
// lock held. Bounded by map size; runs O(n) once per sweepThreshold
// inserts so amortised cost per Set is O(1).
func (c *TTLCache[V]) sweepLocked() {
	now := time.Now()
	for k, v := range c.items {
		if !v.expiresAt.IsZero() && now.After(v.expiresAt) {
			delete(c.items, k)
		}
	}
}

// Purge drops every entry. Used on disconnect / re-connect flows
// where the entire cache scope is invalidated at once.
func (c *TTLCache[V]) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = make(map[string]ttlEntry[V])
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

// Forever is a sentinel for "cache without time-based expiry". Use as
// the defaultTTL argument to NewTTLCache when entries are only ever
// invalidated explicitly.
func Forever() time.Duration { return 0 }
