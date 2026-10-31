//go:build integration
// +build integration

package integration_test

// V-114 (b): a job woken by a process that runs no sweeps (SERVICE_ROLE=api)
// runs at once on one of the processes that do, through a real Redis: every
// worker hears the wake, and one claims it. Three registries stand for the
// three processes, sharing one Redis.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAWakeFromAnAPIProcessRunsOnOneOfTwoWorkersThroughRedis(t *testing.T) {
	integration.SetupRedis(t)
	name := "wake-test-" + uuid.NewString()[:8]
	var runs atomic.Int32
	job := func() sweepBusiness.Job {
		// Hourly, so any run in this test is a wake's.
		return sweepBusiness.Job{Name: name, Every: time.Hour, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}}
	}
	api, a, b := &sweepBusiness.Registry{}, &sweepBusiness.Registry{}, &sweepBusiness.Registry{}
	for _, r := range []*sweepBusiness.Registry{api, a, b} {
		r.Register(job())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartAll(ctx)
	b.StartAll(ctx)
	// Both workers subscribed to the wakes' channel (business/Sweep's
	// wakeChannel) before the api process sends one.
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		subs, err := redisInit.RedisClient.PubSubNumSub(ctx, "sweep:wake").Result()
		if err == nil && subs["sweep:wake"] == 2 {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("two workers run the sweeps, and %d listen for wakes on Redis (%v)", subs["sweep:wake"], err)
		}
	}
	settled := func(want int32, what string) {
		t.Helper()
		for start := time.Now(); runs.Load() < want; time.Sleep(20 * time.Millisecond) {
			if time.Since(start) > 10*time.Second {
				t.Fatalf("%s: the job ran %d times in 10 s, want %d", what, runs.Load(), want)
			}
		}
		time.Sleep(300 * time.Millisecond) // a second worker's run, if it wrongly ran, has started
		if n := runs.Load(); n != want {
			t.Errorf("%s: the job ran %d times, want %d", what, n, want)
		}
	}
	api.Wake(name, "field 1 at 2")
	settled(1, "a wake from the api process, heard by two workers")
	api.Wake(name, "field 1 at 2")
	time.Sleep(300 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Errorf("the same reason, given again, ran the job again (%d runs)", n)
	}
	api.Wake(name, "field 1 at 3")
	settled(2, "a wake for a new reason")
}
