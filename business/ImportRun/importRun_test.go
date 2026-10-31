package business

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A job has one lease at a time, and only that lease gives it back.
func TestALeaseIsTheJobsOnlyOne(t *testing.T) {
	job := uuid.New()
	a, err := Take(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Take(job); !errors.Is(err, ErrRunAlive) {
		t.Fatalf("a second lease while the first was held: %v, want ErrRunAlive", err)
	}
	// Every lease a test takes is given back: one left alive would hold Idle.
	if other, err := Take(uuid.New()); err != nil {
		t.Errorf("another job's lease: %v", err)
	} else {
		other.Release()
	}
	a.Release()
	b, err := Take(job)
	if err != nil {
		t.Fatalf("after the first was given back: %v", err)
	}
	// Given back twice, the first lease still frees nothing of the second's.
	a.Release()
	a.end()
	if _, err := Take(job); !errors.Is(err, ErrRunAlive) {
		t.Errorf("an old lease freed a newer one: %v", err)
	}
	b.Release()
	if c, err := Take(job); err != nil {
		t.Errorf("after the second was given back: %v", err)
	} else {
		c.Release()
	}
}

// A lease whose run never started is given back by the Release deferred where
// it was taken, a panic on the way included (the router recovers panics, so a
// lease left behind would hold the job until a restart).
func TestALeaseWhoseRunNeverStartedIsGivenBack(t *testing.T) {
	job := uuid.New()
	func() {
		defer func() { _ = recover() }()
		l, err := Take(job)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		panic("between the lease and the start")
	}()
	l, err := Take(job)
	if err != nil {
		t.Fatalf("the lease outlived a panic before its run: %v", err)
	}
	l.Release()
	// A lease given back starts nothing.
	if ok, err := l.Start(t.Context(), []string{"planned"}, func() {}); ok || !errors.Is(err, ErrRunAlive) {
		t.Errorf("a released lease started a run: %v %v", ok, err)
	}
}

// Idle answers at once, not having waited, when no import run is alive,
// waits while any lease is held, whichever job's, and answers that it waited
// when the last is given back; a ctx that ends first ends the wait with its
// error.
func TestIdleWaitsForEveryRunToEnd(t *testing.T) {
	// Bounded, so a lease left alive fails the test rather than hanging it.
	first, cancelFirst := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFirst()
	if waited, err := Idle(first); err != nil || waited {
		t.Fatalf("with no run alive: waited %v, %v", waited, err)
	}
	a, err := Take(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Take(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if waited, err := Idle(short); !waited || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("while two runs were alive: waited %v, %v, want a wait the ctx's end ended", waited, err)
	}
	type answer struct {
		waited bool
		err    error
	}
	done, asking := make(chan answer, 1), make(chan struct{})
	go func() {
		close(asking)
		waited, err := Idle(context.Background())
		done <- answer{waited, err}
	}()
	<-asking
	a.Release()
	select {
	case got := <-done:
		t.Fatalf("answered %+v with a run still alive", got)
	case <-time.After(50 * time.Millisecond):
	}
	b.Release()
	select {
	case got := <-done:
		if got.err != nil || !got.waited {
			t.Fatalf("after the last run ended: %+v, want waited", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still waiting after the last run ended")
	}
}
