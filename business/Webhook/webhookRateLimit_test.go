package business

import (
	"testing"
	"time"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// An incoming webhook stays limited while Redis is down: with no client, and
// with one whose calls fail (an outage after boot), which used to let every
// request through.
func TestIncomingWebhooksStayLimitedWhileRedisIsDown(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	for _, c := range []struct {
		name   string
		client *redis.Client
	}{
		{"no client", nil},
		{"a client whose calls fail", redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})},
	} {
		redisInit.RedisClient = c.client
		hook := uuid.NewString()
		for i := 0; i < WebhookRateLimitPerMin; i++ {
			if !CheckWebhookRateLimit(hook) {
				t.Fatalf("%s: request %d refused within the limit", c.name, i+1)
			}
		}
		if CheckWebhookRateLimit(hook) {
			t.Errorf("%s: a request over the limit was let through", c.name)
		}
	}
}
