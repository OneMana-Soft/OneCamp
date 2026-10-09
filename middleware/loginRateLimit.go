package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// loginRateMaxAttempts is the cap inside the window per IP per surface.
// Email + LDAP each get their own bucket; a brute-force on either trips fast.
const loginRateMaxAttempts = 20

// LoginRateLimit returns a middleware that rate-limits POST requests to login
// endpoints by client IP. It uses the registry-backed fixed-window
// limiter (registry.LoginRate, 15-minute window). If Redis is
// unavailable it counts in this process instead (see IPRateLimit).
//
// `kind` separates buckets across surfaces (email, ldap, forgot, etc).
func LoginRateLimit(kind string) func(http.Handler) http.Handler {
	return IPRateLimit(kind, loginRateMaxAttempts, "Too many login attempts. Please try again later.")
}

// IPRateLimit rate-limits mutating requests per (kind, client IP) in the
// registry.LoginRate 15-minute window, answering 429 with msg once max is
// reached.
//
// When Redis can't be asked, the request is counted in this process instead
// (redisStore.AllowFixedWindowOrLocal). It used to be let through, so a Redis
// outage lifted the sign-in, two-step and reset limits, the ones standing
// between a guesser and a password or a six-digit code.
func IPRateLimit(kind string, max int, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Don't gate preflights or non-mutating probes. Every sign-in,
			// two-step and reset route is a POST; the guest, booking and
			// unsubscribe GETs this is also attached to go uncounted, which
			// costs load, not guessing (their tokens are 24 or 32 random bytes).
			if r.Method == http.MethodOptions || r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}

			ip := helpers.ClientIP(r)
			if ip == "" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()

			rctx, cancel := contextWithRedisTimeout(ctx)
			defer cancel()

			res := redisStore.AllowFixedWindowOrLocal(rctx, registry.LoginRate, []string{kind, ip}, max)
			if !res.Allowed {
				retryAfter := res.RetryAfterSeconds()
				if retryAfter <= 0 {
					retryAfter = int(registry.LoginRate.TTL.Seconds())
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{
					"msg":    msg,
					"status": "rate_limited",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// contextWithRedisTimeout wraps the request ctx in a 500ms-bounded timeout
// for our rate-limit Redis call. Long enough that healthy Redis isn't
// affected, short enough that a hung Redis doesn't queue requests.
func contextWithRedisTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 500*time.Millisecond)
}
