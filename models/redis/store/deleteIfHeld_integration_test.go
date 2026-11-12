//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A claim is let go only by the value its holder took it with: another
// value leaves it held, and a key that's gone is no claim to let go
// (V-529 (1): the Tree lock's and a batch's claims).
func TestAClaimIsLetGoOnlyByItsHolder(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	args := []string{"test-" + uuid.NewString()}
	if ok, err := store.SetStringIfAbsent(ctx, registry.TreeLock, args, "mine", time.Minute); !ok || err != nil {
		t.Fatalf("the claim: %v %v", ok, err)
	}
	if let, err := store.DeleteIfHeld(ctx, registry.TreeLock, args, "theirs"); let || err != nil {
		t.Errorf("another holder's value let it go: %v %v", let, err)
	}
	if v, found, _ := store.GetString(ctx, registry.TreeLock, args); !found || v != "mine" {
		t.Fatalf("after another value's try the claim holds %q (%v); want mine", v, found)
	}
	if let, err := store.DeleteIfHeld(ctx, registry.TreeLock, args, "mine"); !let || err != nil {
		t.Errorf("its holder didn't let it go: %v %v", let, err)
	}
	if _, found, _ := store.GetString(ctx, registry.TreeLock, args); found {
		t.Error("the claim is still held after its holder let it go")
	}
	if let, err := store.DeleteIfHeld(ctx, registry.TreeLock, args, "mine"); let || err != nil {
		t.Errorf("a claim that's gone was let go again: %v %v", let, err)
	}
}
