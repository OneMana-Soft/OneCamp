package business

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A registered job runs on its interval once started, keeps its schedule through
// an error and a panic, and stops with the context it was started with.
func TestAJobRunsOnItsIntervalUntilTheContextEnds(t *testing.T) {
	var r Registry
	var runs atomic.Int32
	r.Register(Job{Name: "count", Every: 5 * time.Millisecond, Run: func(context.Context) error {
		switch runs.Add(1) {
		case 1:
			return errors.New("a bad pass")
		case 2:
			panic("a worse one")
		}
		return nil
	}})
	if runs.Load() != 0 {
		t.Fatal("a job runs only once started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.StartAll(ctx)
	r.StartAll(ctx) // a second start starts nothing more
	waitFor(t, func() bool { return runs.Load() >= 4 })
	cancel()
	time.Sleep(20 * time.Millisecond) // a pass already under way may finish
	stopped := runs.Load()
	time.Sleep(30 * time.Millisecond)
	if runs.Load() != stopped {
		t.Errorf("the job kept running after its context ended (%d → %d)", stopped, runs.Load())
	}
}

// After the context a start ran the jobs under has ended, the next start runs
// them again, the first pass one interval after it. The integration tests start
// the sweeps under each test's own context: once one test's context ended, the
// content queue went unread for every later test in the process, and a
// workspace export sat queued until its test gave up.
func TestAStartAfterTheLastOnesEndedRunsTheJobsAgain(t *testing.T) {
	const interval = 25 * time.Millisecond
	var r Registry
	var runs atomic.Int32
	r.Register(Job{Name: "queue", Every: interval, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	first, end := context.WithCancel(context.Background())
	r.StartAll(first)
	waitFor(t, func() bool { return runs.Load() >= 1 })
	end()
	time.Sleep(2 * interval) // a pass already under way may finish
	before := runs.Load()

	second, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartAll(second)
	r.StartAll(second) // a second start under a live context starts nothing more
	// Without the restart no pass ever comes, so the wait is the file's own
	// (waitFor), not a bound in intervals that a loaded machine can miss.
	waitFor(t, func() bool { return runs.Load() > before })
}

// A job registered after the start starts at once, under the same context.
func TestALateJobStartsAtOnce(t *testing.T) {
	var r Registry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartAll(ctx)
	var runs atomic.Int32
	r.Register(Job{Name: "late", Every: 5 * time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	waitFor(t, func() bool { return runs.Load() >= 1 })
}

// A job that could never run, or a second job by the same name, fails at
// registration instead of becoming a sweep that quietly never happens.
func TestABadJobFailsWhereItIsRegistered(t *testing.T) {
	run := func(context.Context) error { return nil }
	for _, c := range []struct {
		name string
		jobs []Job
	}{
		{"no name", []Job{{Every: time.Second, Run: run}}},
		{"no interval", []Job{{Name: "a", Run: run}}},
		{"nothing to run", []Job{{Name: "a", Every: time.Second}}},
		{"a name taken", []Job{{Name: "a", Every: time.Second, Run: run}, {Name: "a", Every: time.Minute, Run: run}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registering did not panic", c.name)
				}
			}()
			var r Registry
			for _, j := range c.jobs {
				r.Register(j)
			}
		}()
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
