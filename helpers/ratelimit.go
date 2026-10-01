package helpers

import (
	"sync"
	"time"
)

// RateLimiter is a fixed-window limiter keyed by whatever the caller counts by:
// an IP, a user id, a workspace. Generic because the alternative is what this
// codebase already had, a bespoke sync.Map and three constants next to the one
// handler that needed it, which the next handler then copies.
//
// Eviction is amortised onto writes rather than given a ticker. The bespoke
// limiter this replaces kept a goroutine and a ticker alive for the life of the
// process whether or not its endpoint was ever called, which is a background
// wakeup every ten minutes on every install, most of them for an empty map.
type RateLimiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	seen map[string]*rateWindow
	// sweepAt is when the next eviction pass may run: a limiter with no traffic
	// should cost nothing, including a timer nobody is waiting on.
	sweepAt time.Time
}

type rateWindow struct {
	count int
	ends  time.Time
}

// NewRateLimiter allows `limit` events per `window` for each key. A limit of zero
// or less allows everything, so a caller can disable one with a config value
// rather than a branch at every call site.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:  limit,
		window: window,
		seen:   make(map[string]*rateWindow),
	}
}

// Allow records one event for key and reports whether it is within the limit.
// An empty key is never limited: a caller that cannot identify who is asking
// must not accidentally rate-limit everyone into one bucket.
func (l *RateLimiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 || key == "" {
		return true
	}

	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	w, ok := l.seen[key]
	if !ok || now.After(w.ends) {
		l.seen[key] = &rateWindow{count: 1, ends: now.Add(l.window)}
		return true
	}

	w.count++
	return w.count <= l.limit
}

// Retry reports how long until the key's current window ends, so a handler can
// tell the caller when to come back instead of leaving them to guess.
func (l *RateLimiter) Retry(key string) time.Duration {
	if l == nil || key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.seen[key]
	if !ok {
		return 0
	}
	if d := time.Until(w.ends); d > 0 {
		return d
	}
	return 0
}

// sweep drops windows that have expired. Caller holds the lock.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Before(l.sweepAt) {
		return
	}
	// One pass per window at most, and only over what is there. Both are cheap
	// next to the request the caller is about to serve.
	l.sweepAt = now.Add(l.window)
	for k, w := range l.seen {
		if now.After(w.ends) {
			delete(l.seen, k)
		}
	}
}
