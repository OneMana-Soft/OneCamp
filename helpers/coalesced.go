package helpers

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// Coalesced runs a job one run at a time, however many callers ask at once.
// A call waits for the next run that starts after it, and every call made
// before that run starts shares it. So a caller whose own work the job must
// see (rows it wrote, for a count of them) is always served by a run that
// began after that work, and a burst of callers costs one run, or two when
// one was already under way. singleflight's join of the run under way would
// serve a caller with a run that started before its work.
//
// V-108 (CR-97): several fields made at once each started their own ANALYZE
// of the cell index, and they queued on its lock.
type Coalesced struct {
	name string
	run  func(context.Context) error

	mu      sync.Mutex
	running bool
	next    *coalescedRun
}

// coalescedRun is one run: done is closed when it ends, with err its error.
type coalescedRun struct {
	done chan struct{}
	err  error
}

// NewCoalesced is a Coalesced of run, named for its logs. run is given a
// context of no caller's: one caller that stops waiting never stops a run
// the others share.
func NewCoalesced(name string, run func(context.Context) error) *Coalesced {
	return &Coalesced{name: name, run: run}
}

// Do waits for the next run of the job to end and returns its error, or
// ctx's error when ctx ends first (the run goes on for the others). A ctx
// that has already ended asks for no run.
func (c *Coalesced) Do(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	r := c.next
	if r == nil {
		r = &coalescedRun{done: make(chan struct{})}
		c.next = r
	}
	if !c.running {
		c.running = true
		GoSafeNamed(c.name, c.loop)
	}
	c.mu.Unlock()
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop runs the job while a run is asked for, one at a time, and ends when
// none is.
func (c *Coalesced) loop() {
	for {
		c.mu.Lock()
		r := c.next
		c.next = nil
		if r == nil {
			c.running = false
		}
		c.mu.Unlock()
		if r == nil {
			return
		}
		r.err = c.once()
		close(r.done)
	}
}

// once is one run. A panic in it is its error, so no caller waits for a run
// that isn't coming, and the next run still starts.
func (c *Coalesced) once() (err error) {
	defer func() {
		if p := recover(); p != nil {
			LogErrorWithContext(context.Background(), "helpers.Coalesced[%s] recovered panic: %v\n%s", c.name, p, debug.Stack())
			err = fmt.Errorf("%s panicked: %v", c.name, p)
		}
	}()
	return c.run(context.Background())
}
