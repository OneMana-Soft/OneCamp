//go:build integration

package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A row budget counts what a call spends, not the call: a batch of n rises the
// counter by n, a call that would pass the limit is refused with the time
// left in its window, and a call that names nothing still spends one.
func TestAllowFixedWindowNSpendsNAndRefusesPastTheLimit(t *testing.T) {
	ctx := context.Background()
	integration.SetupRedis(t)
	token := "rows-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{token}
	if res := store.AllowFixedWindowN(ctx, registry.ApiRowWrites, args, 7, 10); !res.Allowed || res.Count != 7 {
		t.Fatalf("7 of 10: %+v", res)
	}
	if res := store.AllowFixedWindowN(ctx, registry.ApiRowWrites, args, 0, 10); !res.Allowed || res.Count != 8 {
		t.Fatalf("a call that names no rows spends one: %+v", res)
	}
	res := store.AllowFixedWindowN(ctx, registry.ApiRowWrites, args, 3, 10)
	if res.Allowed || res.Count != 11 || res.RetryAfterSeconds() < 1 || res.RetryAfter > registry.ApiRowWrites.TTL {
		t.Errorf("3 more past 10: %+v, want refused with the window's time left", res)
	}
	if res := store.AllowFixedWindowN(ctx, registry.ApiRowReads, args, 10, 10); !res.Allowed {
		t.Errorf("reads are their own counter: %+v", res)
	}
}
