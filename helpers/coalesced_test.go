package helpers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of callers while a run is under way costs one more run, which
// starts after every one of them asked, and each of them is served by it.
func TestCoalescedServesABurstWithOneRunThatStartsAfterIt(t *testing.T) {
	var runs atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	c := NewCoalesced("test.burst", func(context.Context) error {
		n := runs.Add(1)
		started <- struct{}{}
		if n == 1 {
			<-gate
		}
		return fmt.Errorf("run %d", n)
	})
	first := make(chan error, 1)
	go func() { first <- c.Do(context.Background()) }()
	<-started // the first run is under way

	const burst = 10
	var wg sync.WaitGroup
	errs := make([]error, burst)
	in := make(chan struct{}, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Do(&joining{Context: context.Background(), in: in})
		}(i)
	}
	// Every caller of the burst is waiting before the first run ends.
	for i := 0; i < burst; i++ {
		<-in
	}
	close(gate)
	if err := <-first; err == nil || err.Error() != "run 1" {
		t.Errorf("the first caller got %v, where its run was the first", err)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil || err.Error() != "run 2" {
			t.Errorf("caller %d of the burst got %v, where one run after the first serves them all", i, err)
		}
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("%d runs for one caller and a burst of %d during its run, where 2 are", n, burst)
	}
}

// A caller that stops waiting hears why at once, and the run it shares goes
// on for the others, with no caller's context; a caller whose context has
// already ended asks for no run.
func TestCoalescedCallerThatStopsWaitingLeavesTheRunToTheOthers(t *testing.T) {
	var runs atomic.Int32
	gate := make(chan struct{})
	c := NewCoalesced("test.stop", func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			<-gate
		}
		return ctx.Err()
	})
	// A run under way, so the two callers below share the next one.
	held := make(chan error, 1)
	go func() { held <- c.Do(context.Background()) }()
	waitFor(t, func() bool { return runs.Load() == 1 })

	stopped, cancel := context.WithCancel(context.Background())
	in := make(chan struct{}, 2)
	gone := make(chan error, 1)
	go func() { gone <- c.Do(&joining{Context: stopped, in: in}) }()
	other := make(chan error, 1)
	go func() { other <- c.Do(&joining{Context: context.Background(), in: in}) }()
	<-in
	<-in
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Errorf("a caller that stopped waiting got %v", err)
	}
	close(gate)
	if err := <-held; err != nil {
		t.Errorf("the run under way: %v", err)
	}
	if err := <-other; err != nil {
		t.Errorf("the run went on with a stopped caller's context: %v", err)
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("%d runs, where the one under way and the one the two callers shared are 2", n)
	}

	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.running
	})
	if err := c.Do(stopped); !errors.Is(err, context.Canceled) {
		t.Errorf("an ended context: %v, where it hears that it ended", err)
	}
	c.mu.Lock()
	asked := c.running || c.next != nil
	c.mu.Unlock()
	if asked {
		t.Error("an ended context asked for a run")
	}
}

// A run that panics is its callers' error, and the next run still starts.
func TestCoalescedPanicIsTheRunsError(t *testing.T) {
	var runs atomic.Int32
	c := NewCoalesced("test.panic", func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("subsystem is on fire")
		}
		return nil
	})
	if err := c.Do(context.Background()); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("a run that panicked: %v, where its callers hear it", err)
	}
	if err := c.Do(context.Background()); err != nil {
		t.Errorf("the run after a panic: %v", err)
	}
}

// joining is a caller's context that says when the caller has joined a run:
// Do reads Done only once it has a run to wait for.
type joining struct {
	context.Context
	in   chan<- struct{}
	once sync.Once
}

func (j *joining) Done() <-chan struct{} {
	j.once.Do(func() { j.in <- struct{}{} })
	return j.Context.Done()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
