package business

import (
	"errors"
	"testing"

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
	if _, err := Take(uuid.New()); err != nil {
		t.Errorf("another job's lease: %v", err)
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
	if _, err := Take(job); err != nil {
		t.Errorf("after the second was given back: %v", err)
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
