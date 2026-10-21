package helpers

import (
	"sync"
	"time"
)

// Throttle runs work for a key at most once a window: the first request at
// once, and any made while that key's window is open once more when it
// closes, so the last change in a burst is always run, and a burst of a
// hundred is two runs. The work must read the newest state itself: a trailing
// run is the first request's run function, called again.
//
// Each run happens on a goroutine of its own (GoSafeNamed). Nothing is kept
// for a key whose window closed with nothing due, so a throttle over many keys
// holds only the busy ones.
//
// Settle waits for every open window to close, trailing runs included, which
// is how a test, or a server shutting down, waits for the work to be done.
type Throttle struct {
	name   string
	window time.Duration
	mu     sync.Mutex
	due    map[string]bool // key → its window is open; true: a run is due when it closes
	open   Inflight        // windows open
}

// NewThrottle is a throttle whose runs are named name in the logs.
func NewThrottle(name string, window time.Duration) *Throttle {
	return &Throttle{name: name, window: window, due: map[string]bool{}}
}

// Do asks for run to happen for key: now, if key's window is closed, or once
// when it closes.
func (t *Throttle) Do(key string, run func()) {
	t.mu.Lock()
	if _, open := t.due[key]; open {
		t.due[key] = true
		t.mu.Unlock()
		return
	}
	t.due[key] = false
	t.open.Add(1)
	t.mu.Unlock()
	t.runAndWait(key, run)
}

func (t *Throttle) runAndWait(key string, run func()) {
	GoSafeNamed(t.name, run)
	time.AfterFunc(t.window, func() {
		t.mu.Lock()
		if !t.due[key] {
			delete(t.due, key)
			t.mu.Unlock()
			t.open.Add(-1)
			return
		}
		t.due[key] = false
		t.mu.Unlock()
		t.runAndWait(key, run)
	})
}

// Settle waits until every open window has closed, for at most wait, and says
// whether they all have.
func (t *Throttle) Settle(wait time.Duration) bool { return t.open.Settle(wait) }
