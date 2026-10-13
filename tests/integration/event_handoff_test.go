//go:build integration
// +build integration

package integration_test

// A server split into an api and workers, against real Redis (BE-BUG-9).
//
// Every event listener (agents, the AI coworker, workflows, the Slack bridge)
// registers with the background loops, which an api-only server never runs, so
// the events its requests raised reached nobody. Now such a server hands each
// event to the workers through a Redis stream, and they run their listeners on
// it:
//   - nothing runs where the event was raised, and it waits for a worker;
//   - a worker's listener gets it with its data, and with where it came from
//     (a GitHub change is never taken to be a member's ask);
//   - a listener that fails is tried again, and one that already had the
//     event doesn't get it twice;
//   - the system check says whether a worker is taking events.
//
// One process plays both parts: hand-off on is the api, the drain is the
// worker.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEventsCrossFromAnAPIServerToAWorker -v

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEventsCrossFromAnAPIServerToAWorker(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err) // outgoing webhooks are read from Postgres on every dispatch
	}
	integration.SetupRedis(t)
	rdb := redisInit.RedisClient

	marker := uuid.NewString()
	type heard struct {
		kind   string
		data   map[string]interface{}
		github bool
	}
	var mu sync.Mutex
	var steady []heard // every event of this test the steady listener had
	var flakyTries, flakyDone atomic.Int32
	// The two listeners a worker runs, each ignoring other tests' events.
	webhookBusiness.RegisterEventListener(func(c context.Context, kind string, data map[string]interface{}) {
		if data["marker"] != marker {
			return
		}
		mu.Lock()
		steady = append(steady, heard{kind, data, helpers.IsGitHubOrigin(c)})
		mu.Unlock()
	})
	webhookBusiness.RegisterEventListener(func(_ context.Context, kind string, data map[string]interface{}) {
		if data["marker"] != marker || kind != "post.updated" {
			return
		}
		if flakyTries.Add(1) == 1 {
			panic("the first try fails")
		}
		flakyDone.Add(1)
	})
	steadyHeard := func(kind string) []heard {
		mu.Lock()
		defer mu.Unlock()
		var out []heard
		for _, h := range steady {
			if h.kind == kind {
				out = append(out, h)
			}
		}
		return out
	}

	undo := webhookBusiness.HandEventsToWorkers()
	defer undo()

	t.Run("an event raised on the api waits for a worker", func(t *testing.T) {
		webhookBusiness.DispatchEvent(helpers.WithGitHubOrigin(ctx), "post.created", map[string]interface{}{
			"marker": marker, "post_id": "p-1", "mention_ids": []string{"0x1", "u-2"},
		})
		time.Sleep(300 * time.Millisecond)
		if got := steadyHeard("post.created"); len(got) != 0 {
			t.Fatalf("a listener ran where the event was raised: %+v", got)
		}
		if n, err := rdb.XLen(ctx, "events:handoff").Result(); err != nil || n != 1 {
			t.Fatalf("%d events waiting (err %v), want 1", n, err)
		}
	})

	drainCtx, stopDrain := context.WithCancel(ctx)
	defer stopDrain()
	webhookBusiness.StartEventDrain(drainCtx)

	t.Run("a worker's listener gets it, with where it came from", func(t *testing.T) {
		waitFor(t, "the worker to run the event", func() bool { return len(steadyHeard("post.created")) == 1 })
		h := steadyHeard("post.created")[0]
		ids, _ := h.data["mention_ids"].([]interface{})
		if h.data["post_id"] != "p-1" || len(ids) != 2 || ids[0] != "0x1" || ids[1] != "u-2" {
			t.Errorf("data %#v", h.data)
		}
		if !h.github {
			t.Error("a change from GitHub reached the listener as a member's")
		}
		waitFor(t, "the event to be acknowledged", func() bool {
			p, err := rdb.XPending(ctx, "events:handoff", "listeners").Result()
			return err == nil && p.Count == 0
		})
	})

	t.Run("a listener that fails is tried again, and only it", func(t *testing.T) {
		webhookBusiness.DispatchEvent(ctx, "post.updated", map[string]interface{}{"marker": marker, "post_id": "p-1"})
		// The retry waits out its backoff (10 s) and the next sweep (5 s).
		waitLong(t, "the failing listener's second try", 40*time.Second, func() bool { return flakyDone.Load() == 1 })
		if n := flakyTries.Load(); n != 2 {
			t.Errorf("the failing listener ran %d times, want 2", n)
		}
		if got := steadyHeard("post.updated"); len(got) != 1 {
			t.Errorf("the listener that had it ran %d times, want once", len(got))
		}
		waitFor(t, "the event to be acknowledged", func() bool {
			p, err := rdb.XPending(ctx, "events:handoff", "listeners").Result()
			return err == nil && p.Count == 0
		})
		if keys, _ := rdb.Keys(ctx, "events:handoff:done:*").Result(); len(keys) != 0 {
			t.Errorf("left %v behind", keys)
		}
	})

	check := func() helpers.SystemCheckResult {
		for _, r := range helpers.RunSystemChecks(ctx, 3*time.Second) {
			if r.Name == "event-listeners" {
				return r
			}
		}
		t.Fatal("no event-listeners check")
		return helpers.SystemCheckResult{}
	}
	t.Run("the system check says whether a worker takes events", func(t *testing.T) {
		if r := check(); !r.Healthy {
			t.Fatalf("unhealthy with a worker taking events: %s", r.Detail)
		}
		stopDrain()
		time.Sleep(6 * time.Second) // past one sweep, so the drain is gone
		if err := rdb.Del(ctx, "events:handoff:alive").Err(); err != nil {
			t.Fatal(err)
		}
		if r := check(); r.Healthy {
			t.Fatal("healthy with no worker taking events")
		}
	})
}

// waitLong is waitFor with a deadline of the caller's.
func waitLong(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
