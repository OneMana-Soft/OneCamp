package helpers

// Work that runs on after the call that started it, and how to wait for it.
//
// A save hands work off and returns, such as an audit row to the chain's
// writer. Two kinds of caller need to know when that work is done. A server shutting down, so what it queued isn't lost with the
// process. And the integration tests, so one test's work can't land in the
// next test's database or run after the clients it uses were taken down: a
// goroutine that did once panicked and took the whole test binary with it.
//
// So each package that hands work off registers how to wait for its own, from
// an init(), and SettleBackground waits for all of it without importing any of
// them. Linking a package is what registers its work, so the AI edition's
// writers are waited for where they exist and nowhere else.

import (
	"sort"
	"sync"
	"time"
)

// Inflight counts work in flight and lets a caller wait until there is none.
// The zero value is ready to use.
type Inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n falls to 0; replaced when it rises from 0
}

// Add changes the count by delta, which is negative when work finishes (as
// sync.WaitGroup's Add). A count below zero is a programming error and panics.
func (f *Inflight) Add(delta int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	was := f.n
	f.n += delta
	switch {
	case f.n < 0:
		panic("helpers.Inflight: more work finished than started")
	case was == 0 && f.n > 0:
		f.idle = make(chan struct{})
	case was > 0 && f.n == 0:
		close(f.idle)
	}
}

// Settle waits until nothing is in flight, for at most wait, and says whether
// nothing is. Work started while it waits is waited for too.
func (f *Inflight) Settle(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		n, idle := f.n, f.idle
		f.mu.Unlock()
		if n == 0 {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		t := time.NewTimer(left)
		select {
		case <-idle:
			t.Stop()
		case <-t.C:
			return false
		}
	}
}

type backgroundWork struct {
	name   string
	settle func(wait time.Duration) bool
}

var (
	backgroundMu   sync.Mutex
	backgroundList = map[string]func(wait time.Duration) bool{}
)

// RegisterBackground names work that runs on after the call that started it,
// with how to wait for it: settle waits at most wait and says whether the work
// is done. Call it from an init(). Registering a name again replaces it.
func RegisterBackground(name string, settle func(wait time.Duration) bool) {
	if name == "" || settle == nil {
		return
	}
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	backgroundList[name] = settle
}

// SettleBackground waits until no registered work is in flight, for at most
// wait, and returns the names of the work still running then (none when it
// all settled), in name order.
//
// It goes round until one pass finds all of it idle at once, because settling
// one kind can start another: an event's sink writes an audit row, and a doc's
// save writes its search entry and then its AI entry.
func SettleBackground(wait time.Duration) []string {
	deadline := time.Now().Add(wait)
	for {
		var busy []string
		waited := false
		for _, w := range registeredBackground() {
			if w.settle(0) {
				continue
			}
			waited = true
			if !w.settle(time.Until(deadline)) {
				busy = append(busy, w.name)
			}
		}
		if !waited || len(busy) > 0 {
			return busy
		}
	}
}

func registeredBackground() []backgroundWork {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	out := make([]backgroundWork, 0, len(backgroundList))
	for name, settle := range backgroundList {
		out = append(out, backgroundWork{name, settle})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
