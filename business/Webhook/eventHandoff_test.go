package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// What a listener reads from the dispatching context survives the crossing:
// an agent run must not be taken to be asked for by a member when the words
// came from GitHub or a public form (business/AIAgent eventAsker).
func TestHandoffKeepsWhereAnEventCameFrom(t *testing.T) {
	ctx := helpers.WithGitHubOrigin(context.Background())
	b, err := json.Marshal(handedOffEvent{V: 1, Kind: "post.created", Origin: originOf(ctx)})
	if err != nil {
		t.Fatal(err)
	}
	var back handedOffEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := back.Origin.context()
	if !helpers.IsGitHubOrigin(got) {
		t.Fatalf("origin %+v came back without github", back.Origin)
	}
	if helpers.IsGitHubOrigin(handoffOrigin{}.context()) {
		t.Fatal("a member's event gained an origin on the way")
	}
}

// The event's data arrives as JSON, which every listener reads: a list as
// []interface{} of strings (mentionIDsFromEvent, toStringSlice, mentionsBot),
// a flag as a bool.
func TestHandoffDataReadsAsListenersExpect(t *testing.T) {
	data := map[string]interface{}{"post_id": "p1", "mention_ids": []string{"0x1", "u-2"}, "merged": true}
	b, err := json.Marshal(handedOffEvent{V: 1, Kind: "post.created", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	var back handedOffEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	ids, ok := back.Data["mention_ids"].([]interface{})
	if !ok || len(ids) != 2 || ids[0] != "0x1" || ids[1] != "u-2" {
		t.Fatalf("mention_ids came back as %#v", back.Data["mention_ids"])
	}
	if merged, _ := back.Data["merged"].(bool); !merged || back.Data["post_id"] != "p1" {
		t.Fatalf("data came back as %#v", back.Data)
	}
}

func TestHandoffBackoffDoublesToItsCeiling(t *testing.T) {
	want := map[int64]time.Duration{0: 10 * time.Second, 1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 6: 320 * time.Second, 7: 10 * time.Minute, 40: 10 * time.Minute}
	for n, d := range want {
		if got := handoffBackoff(n); got != d {
			t.Errorf("after %d deliveries wait %s, want %s", n, got, d)
		}
	}
}

func listenerA(context.Context, string, map[string]interface{}) {}
func listenerB(context.Context, string, map[string]interface{}) {}

// A worker that takes an event over knows which listeners had it by name, so
// the name is the function's, the same in every process of one build.
func TestListenersAreNamedByTheirFunction(t *testing.T) {
	listenerMu.Lock()
	saved := listeners
	listeners = []EventListener{listenerA, listenerB, listenerA}
	listenerMu.Unlock()
	t.Cleanup(func() {
		listenerMu.Lock()
		listeners = saved
		listenerMu.Unlock()
	})

	got := currentListeners()
	if len(got) != 3 {
		t.Fatalf("%d listeners", len(got))
	}
	a, b, a2 := got[0].name, got[1].name, got[2].name
	if !strings.HasSuffix(a, "/business/Webhook.listenerA") || !strings.HasSuffix(b, "/business/Webhook.listenerB") || a2 != a+"#2" {
		t.Fatalf("named %q, %q, %q", a, b, a2)
	}
}

// Only a process that hands its events over sends them anywhere; one that
// does never runs its own listeners as well, which would run them twice.
func TestAProcessHandsOverOrRunsItsListenersNeverBoth(t *testing.T) {
	ran := make(chan string, 4)
	listenerMu.Lock()
	saved := listeners
	listeners = []EventListener{func(_ context.Context, kind string, _ map[string]interface{}) { ran <- kind }}
	listenerMu.Unlock()
	t.Cleanup(func() {
		listenerMu.Lock()
		listeners = saved
		listenerMu.Unlock()
	})
	savedRedis := redisInit.RedisClient
	redisInit.RedisClient = nil // a hand-off that fails says so, and runs nothing here
	t.Cleanup(func() { redisInit.RedisClient = savedRedis })

	notifyEventListeners(context.Background(), "post.created", map[string]interface{}{})
	select {
	case kind := <-ran:
		if kind != "post.created" {
			t.Fatalf("ran %s", kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a process that runs its listeners didn't")
	}

	undo := HandEventsToWorkers()
	defer undo()
	notifyEventListeners(context.Background(), "post.created", map[string]interface{}{})
	select {
	case kind := <-ran:
		t.Fatalf("ran %s here as well as handing it over", kind)
	case <-time.After(200 * time.Millisecond):
	}
	if err := probeEventHandoff(context.Background()); err == nil {
		t.Fatal("the system check passed a split server with no way to reach a worker")
	}
	undo()
	if err := probeEventHandoff(context.Background()); err != nil {
		t.Fatalf("the system check failed a server that runs its own listeners: %v", err)
	}
}
