//go:build integration

package idempotency

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A send whose hold on its key ran out (it outlived claimFor) lets go of
// nothing when it ends: the send that claimed the key after it still holds
// it, so a third meanwhile is refused as still on its way (V-529 (1)).
func TestASendWhoseHoldRanOutNeverLetsGoOfTheNextOnes(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	caller, route, key := "member:"+uuid.NewString(), "tables.fields.batch", "k-"+uuid.NewString()
	first, err := OpenItems(ctx, caller, route, key, "h")
	if err != nil {
		t.Fatalf("the first send: %v", err)
	}
	// Its hold runs out while it is still applying.
	if err := redisStore.Delete(ctx, registry.ApiBatchClaim, []string{caller, route, key}); err != nil {
		t.Fatal(err)
	}
	second, err := OpenItems(ctx, caller, route, key, "h")
	if err != nil {
		t.Fatalf("the send after the first's hold ran out: %v", err)
	}
	first.Release(ctx)
	if third, err := OpenItems(ctx, caller, route, key, "h"); !errors.Is(err, ErrItemsInProgress) {
		if third != nil {
			third.Release(ctx)
		}
		t.Fatalf("a send while the second applies: %v; want it refused as still on its way (the first let go of the second's hold)", err)
	}
	second.Release(ctx)
	third, err := OpenItems(ctx, caller, route, key, "h")
	if err != nil {
		t.Fatalf("once the second let go, a send: %v; want the key free", err)
	}
	third.Release(ctx)
}

// A send that stopped before any item of its batch was recorded is still
// known to the next send of that batch: its record is kept when the key is
// claimed, so the next send knows one came before it, and can tell a delete
// it finds done from one that never was (V-529 (1)).
func TestASendIsKnownToTheNextEvenWhenItRecordedNoItem(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	caller, route, key := "member:"+uuid.NewString(), "tables.views.batch", "k-"+uuid.NewString()
	first, err := OpenItems(ctx, caller, route, key, "h")
	if err != nil {
		t.Fatalf("the first send: %v", err)
	}
	if first.Again() {
		t.Errorf("the first send of a batch says one came before it")
	}
	first.Release(ctx) // it stopped before its first item was recorded
	second, err := OpenItems(ctx, caller, route, key, "h")
	if err != nil {
		t.Fatalf("the batch sent again: %v", err)
	}
	defer second.Release(ctx)
	if !second.Again() {
		t.Errorf("the batch sent again doesn't know a send came before it")
	}
	if _, err := OpenItems(ctx, caller, route, key, "another"); !errors.Is(err, ErrItemsInProgress) {
		t.Errorf("another batch while the second applies: %v; want in progress", err)
	}
}

// A batch refused at its checks applied nothing, so its key isn't used up:
// the corrected batch under it runs as a first send (CR-220 F1).
func TestABatchRefusedLeavesItsKeyForTheCorrectedOne(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	caller, route, key := "member:"+uuid.NewString(), "tables.fields.batch", "k-"+uuid.NewString()
	refused, err := OpenItems(ctx, caller, route, key, "h1")
	if err != nil {
		t.Fatalf("the first send: %v", err)
	}
	refused.Forget(ctx)
	refused.Release(ctx)
	corrected, err := OpenItems(ctx, caller, route, key, "h2")
	if err != nil {
		t.Fatalf("the corrected batch under the refused one's key: %v; want it run", err)
	}
	defer corrected.Release(ctx)
	if corrected.Again() {
		t.Error("the corrected batch says a send of it came before")
	}
}
