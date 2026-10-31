package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// Every public read's one per-address limit (B6 of the 22 Oct audit): the
// middleware's and the guest kinds' were two copies of this, with two 429s.
func TestAllowPerAddressCountsEachSurfaceApartAndLetsNoAddressThrough(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as when Redis is down
	ctx := context.Background()
	// A surface of this run's own, so a counter left by another run never counts.
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	ip := "198.51.100.23"
	for i := 1; i <= 2; i++ {
		if !AllowPerAddress(ctx, "page-"+run, ip, 2).Allowed {
			t.Fatalf("ask %d of 2 was refused", i)
		}
	}
	if res := AllowPerAddress(ctx, "page-"+run, ip, 2); res.Allowed || res.RetryAfterSeconds() < 1 {
		t.Errorf("the third ask in the minute: %+v", res)
	}
	if !AllowPerAddress(ctx, "room-"+run, ip, 2).Allowed {
		t.Error("another surface was charged the page's asks")
	}
	for i := 0; i < 3; i++ {
		if !AllowPerAddress(ctx, "page-"+run, "", 2).Allowed {
			t.Fatal("an ask with no address was counted")
		}
	}
}
