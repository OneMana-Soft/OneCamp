//go:build integration
// +build integration

package integration_test

// The docs programme's events with a real Redis (A6): thirty saves of a doc in
// a minute fire one doc.updated webhook, gathered across instances in Redis and
// flushed by the claimed sweep; and a sweep two instances tick runs once a slot.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestThirtySaves|TestASweepClaimed' -v

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestThirtySavesInAMinuteFireOneWebhook(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	doc := uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch notes",
		"doc_private": false, "doc_deleted_at": "0001-01-01T00:00:00Z"}})

	var mu sync.Mutex
	var fired []map[string]interface{}
	// In-process listeners hear every webhook event DispatchEvent sends out.
	webhookBusiness.RegisterEventListener(func(_ context.Context, eventType string, data map[string]interface{}) {
		if eventType == "doc.updated" && data["doc_uuid"] == doc {
			mu.Lock()
			fired = append(fired, data)
			mu.Unlock()
		}
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(fired)
	}

	for i := 0; i < 30; i++ {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: doc})
		time.Sleep(20 * time.Millisecond)
	}
	integration.Settle(t)
	if n := count(); n != 0 {
		t.Fatalf("%d webhooks fired before the minute closed", n)
	}

	// The webhook sweep, in a registry of its own: the server's registry
	// starts once a process, and another test may start it.
	sweeps, stop := context.WithCancel(ctx)
	defer stop()
	own := &sweepBusiness.Registry{}
	own.Register(followBusiness.DocUpdatedWebhooks)
	own.StartAll(sweeps)
	deadline := time.Now().Add(110 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	time.Sleep(17 * time.Second) // one more pass of the sweep
	if n := count(); n != 1 {
		t.Fatalf("thirty saves in a minute fired %d webhooks, want 1", n)
	}
	mu.Lock()
	got := fired[0]
	mu.Unlock()
	if got["doc_title"] != "Launch notes" || got["doc_private"] != false {
		t.Errorf("the webhook carried %v", got)
	}
}

func TestASweepClaimedByOneOfTwoInstancesRunsOnce(t *testing.T) {
	integration.SetupRedis(t)
	name := "claim-test-" + uuid.NewString()[:8]
	var runs atomic.Int32
	job := func() sweepBusiness.Job {
		return sweepBusiness.Job{Name: name, Every: time.Second, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}}
	}
	// Two instances: two registries, the same job, one Redis.
	a, b := &sweepBusiness.Registry{}, &sweepBusiness.Registry{}
	a.Register(job())
	b.Register(job())
	ctx, cancel := context.WithCancel(context.Background())
	a.StartAll(ctx)
	time.Sleep(400 * time.Millisecond) // out of phase
	b.StartAll(ctx)
	time.Sleep(4200 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	// Each instance ticked four times; unclaimed, that's eight runs. Claimed,
	// one run a one-second slot: four or five.
	if n := runs.Load(); n < 3 || n > 5 {
		t.Errorf("two instances ran the sweep %d times in about 4.6 seconds, want one run a second", n)
	}
}
