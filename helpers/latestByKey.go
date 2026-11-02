package helpers

import (
	"sync"
	"time"
)

// latestWork is every LatestByKey run that is due or running, so a shutdown or a test waits for it (background.go):
// a run dropped at shutdown would leave the newest value unwritten.
var latestWork Inflight

func init() { RegisterBackground("runs due with LatestByKey", latestWork.Settle) }

// LatestByKey runs work for a key with the value asked for it last, one run per key at a time. The first Ask for a key
// waits `wait`, so a burst of asks runs once, then runs with the latest value; an Ask while that key's run is in flight
// is kept, and runs once more after it, after the same wait. Later asks never push a due run back, so a key asked
// without pause still runs once per wait. A key never waits for another key's run. It suits writes that only the newest
// state matters to, such as a board's text after its collaborative saves (business/Board). A run's panic is recovered
// and logged (GoSafeNamed), and the key keeps working. A key is forgotten once its runs are done.
type LatestByKey[K comparable, V any] struct {
	name string
	wait time.Duration
	run  func(K, V)
	mu   sync.Mutex
	keys map[K]*latestOf[V]
	work Inflight
}

type latestOf[V any] struct {
	value   V
	due     bool // a value waits for its run
	running bool
}

// NewLatestByKey makes a LatestByKey that runs run(key, latest value) at most once per wait per key; name labels a
// run's recovered panic in the logs.
func NewLatestByKey[K comparable, V any](name string, wait time.Duration, run func(K, V)) *LatestByKey[K, V] {
	return &LatestByKey[K, V]{name: name, wait: wait, run: run, keys: map[K]*latestOf[V]{}}
}

// Ask keeps v as key's latest value and makes sure a run takes it.
func (l *LatestByKey[K, V]) Ask(key K, v V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.keys[key]
	if e == nil {
		e = &latestOf[V]{}
		l.keys[key] = e
		l.work.Add(1)
		latestWork.Add(1)
	}
	e.value = v
	if e.due {
		return // the due run takes this value
	}
	e.due = true
	if !e.running {
		l.after(key)
	}
	// While a run is in flight, its end starts the next wait (done).
}

// Settle waits until no run is due or running, for at most wait, and says whether it got there.
func (l *LatestByKey[K, V]) Settle(wait time.Duration) bool { return l.work.Settle(wait) }

// after starts key's next run once the wait is over. l.mu is held.
func (l *LatestByKey[K, V]) after(key K) {
	time.AfterFunc(l.wait, func() {
		l.mu.Lock()
		e := l.keys[key]
		v := e.value
		e.due, e.running = false, true
		l.mu.Unlock()
		GoSafeNamed("LatestByKey "+l.name, func() {
			defer l.done(key)
			l.run(key, v)
		})
	})
}

// done ends key's run: the next wait if a value came meanwhile, else the key is forgotten.
func (l *LatestByKey[K, V]) done(key K) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.keys[key]
	e.running = false
	if e.due {
		l.after(key)
		return
	}
	delete(l.keys, key)
	l.work.Add(-1)
	latestWork.Add(-1)
}
