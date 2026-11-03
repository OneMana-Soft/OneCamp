package store

import (
	"context"
	"testing"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
)

// A row budget never takes the API down: when Redis can't be asked, a call
// that spends n goes through, unchecked, as AllowFixedWindow lets one through.
func TestAllowFixedWindowNLetsACallThroughWhenRedisIsDown(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil
	res := AllowFixedWindowN(context.Background(), registry.ApiRowWrites, []string{"token-down"}, 1000, 10)
	if !res.Allowed || !res.Unchecked {
		t.Errorf("Redis down: %+v, want allowed and unchecked", res)
	}
}
