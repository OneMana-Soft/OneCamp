package middleware

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// posts sends n POSTs from addr through IPRateLimit(kind, max) and returns
// their status codes.
func posts(t *testing.T, kind string, max int, addr string, n int) []int {
	t.Helper()
	h := IPRateLimit(kind, max, "slow down")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	codes := make([]int, n)
	for i := range codes {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		req.RemoteAddr = addr + ":4242"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes[i] = rec.Code
		if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: a 429 with no Retry-After", kind)
		}
	}
	return codes
}

// The sign-in, two-step and reset limits used to let everything through when
// Redis couldn't be asked; they now count in the process instead.
func TestSignInLimitsHoldWhileRedisIsDown(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })

	for _, c := range []struct {
		name   string
		client *redis.Client
	}{
		// Never connected: tests, or a process started without Redis.
		{"no client", nil},
		// The server's case: boot needs Redis, so in an outage the client
		// is there and every call to it fails.
		{"a client whose calls fail", redis.NewClient(&redis.Options{
			Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond,
		})},
	} {
		redisInit.RedisClient = c.client
		kind := "test-" + c.name

		want := []int{200, 200, 200, 429, 429}
		if got := posts(t, kind, 3, "203.0.113.7", 5); !slices.Equal(got, want) {
			t.Errorf("%s: codes %v, want %v", c.name, got, want)
		}
		// One address's attempts aren't another's.
		if got := posts(t, kind, 3, "203.0.113.8", 1); got[0] != http.StatusOK {
			t.Errorf("%s: another address was refused: %d", c.name, got[0])
		}
		// Nor are one surface's another's.
		if got := posts(t, kind+"-other", 3, "203.0.113.7", 1); got[0] != http.StatusOK {
			t.Errorf("%s: another surface was refused: %d", c.name, got[0])
		}
		if c.client != nil {
			_ = c.client.Close()
		}
	}
}
