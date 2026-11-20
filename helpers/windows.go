package helpers

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Windows gathers what is added under one key from its first Add until Wait
// has passed, then hands it all to Flush at once: what the many jobs of one
// action tell, told together (business/Task: one bulk change and the rule runs
// it sets going push and email each assignee once). A window handed over is
// over: what comes after opens the next one.
//
// In memory, in one process: what a process gathers, it hands over. An open
// window is work in flight (windowWork), so a graceful shutdown waits for it
// to be handed over; only a crash or a hard kill loses what a window held.
// Flush runs on the timer's goroutine, outside the lock (it may Add again), so
// a Flush that can panic guards itself.
type Windows[T any] struct {
	Wait  time.Duration
	Flush func(key string, items []T)
	// Start runs f once wait has passed; nil is time.AfterFunc. A seam for
	// tests.
	Start func(wait time.Duration, f func())

	mu   sync.Mutex
	open map[string]*window[T]
}

type window[T any] struct{ items []T }

// windowWork is every window open, from its first Add until its Flush has
// returned, so a shutdown or a test waits for it (background.go). A settle
// waits for a window's time to be up and never closes one early: an action's
// later batches still join it.
var windowWork Inflight

func init() { RegisterBackground("windows open with Windows", windowWork.Settle) }

// Add puts items in key's open window, opening one when there is none. Adding
// nothing opens nothing.
func (w *Windows[T]) Add(key string, items ...T) {
	if len(items) == 0 {
		return
	}
	w.mu.Lock()
	if win, ok := w.open[key]; ok {
		win.items = append(win.items, items...)
		w.mu.Unlock()
		return
	}
	if w.open == nil {
		w.open = map[string]*window[T]{}
	}
	win := &window[T]{items: slices.Clone(items)}
	w.open[key] = win
	windowWork.Add(1)
	start := w.Start
	w.mu.Unlock()
	if start == nil {
		start = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	start(w.Wait, func() { w.close(key, win) })
}

// close hands win's items over, once, while it is still key's open window.
func (w *Windows[T]) close(key string, win *window[T]) {
	w.mu.Lock()
	if w.open[key] != win {
		w.mu.Unlock()
		return
	}
	delete(w.open, key)
	items := win.items
	w.mu.Unlock()
	defer windowWork.Add(-1)
	w.Flush(key, items)
}

type originKey struct{}

// WithOrigin marks ctx as one action's, named id: a change someone made once
// that sets many jobs going (a bulk change, and the rule runs its events set
// going, which keep the context's values), so what they tell can be told
// together (Windows). An origin ctx has already stays: the action is the
// outermost one. An empty id marks nothing.
func WithOrigin(ctx context.Context, id string) context.Context {
	if _, ok := OriginOf(ctx); ok || id == "" {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, id)
}

// OriginOf is the action ctx is one of (WithOrigin), if any.
func OriginOf(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, _ := ctx.Value(originKey{}).(string)
	return id, id != ""
}
