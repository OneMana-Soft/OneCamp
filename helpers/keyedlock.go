package helpers

import "sync"

// KeyedLock provides per-key mutual exclusion with a bounded number of waiters.
// Work on the SAME key runs one at a time (serialized, in submission order
// under normal load) while DIFFERENT keys proceed concurrently — without a
// global lock.
//
// It is the in-process primitive for "serialize per conversation / per
// resource". For example, answering one AI turn at a time per DM so replies
// never overlap or arrive out of order, while different DMs run in parallel.
// This mirrors the way mature chat platforms process a bot's messages: a real
// user turn is never dropped just because the bot is busy elsewhere; turns for
// one conversation are simply handled one after another.
//
// The zero value is ready to use. A per-key entry is reference-counted and
// freed once nobody holds or waits on it, so a long-lived process that touches
// many distinct keys does not leak entries.
type KeyedLock struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	mu sync.Mutex
	// count is holders + waiters for this key. It both reference-counts the
	// entry (freed at 0) and bounds the backlog (Acquire's cap check).
	count int
}

// Acquire blocks until the caller holds the lock for key, then returns true;
// the caller MUST call Release(key) exactly once (typically via defer).
//
// maxInFlight bounds how many goroutines may hold-or-wait on the same key at
// once: when the cap is already reached Acquire returns false WITHOUT taking
// the lock, so the caller can shed load (backpressure against a flood) instead
// of piling up unbounded goroutines. maxInFlight <= 0 means unbounded. The cap
// counts the in-progress holder too, so maxInFlight = N allows 1 running plus
// N-1 queued.
func (k *KeyedLock) Acquire(key string, maxInFlight int) bool {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = make(map[string]*keyedLockEntry)
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedLockEntry{}
		k.entries[key] = e
	}
	if maxInFlight > 0 && e.count >= maxInFlight {
		// At capacity: do not enqueue. Free a freshly-created empty entry so a
		// rejected first caller does not leak it.
		if e.count == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
		return false
	}
	e.count++
	k.mu.Unlock()

	e.mu.Lock() // blocks until the previous holder of this key releases
	return true
}

// Release frees the lock for key. It must be paired with a successful Acquire.
func (k *KeyedLock) Release(key string) {
	k.mu.Lock()
	e := k.entries[key]
	if e == nil {
		k.mu.Unlock()
		return
	}
	e.count--
	if e.count <= 0 {
		delete(k.entries, key)
	}
	k.mu.Unlock()

	e.mu.Unlock()
}
