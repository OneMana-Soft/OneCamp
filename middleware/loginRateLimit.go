package middleware

import (
	"context"
	"net/http"
	"strconv"

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

// oauthRegisterMaxAttempts is higher than a login's on purpose: a hosted
// client (Claude, ChatGPT) registers from a small set of egress addresses on
// behalf of everyone in the workspace, so a team connecting on the same
// morning shares one bucket.
const oauthRegisterMaxAttempts = 120

// OAuthRegisterRateLimit bounds dynamic client registration per address.
func OAuthRegisterRateLimit() func(http.Handler) http.Handler {
	return IPRateLimit("oauth_register", oauthRegisterMaxAttempts, "Too many client registrations. Please try again later.")
}

// IPRateLimit rate-limits requests per (kind, client IP): its writes in the
// registry.LoginRate 15-minute window, answering 429 with msg once max is
// reached, and its reads (GET, HEAD) apart, at a link page's budget a minute
// (LinkReadLimit). Reads went uncounted, so the guest links' GETs behind it
// (a meeting, a table, a channel, a project) had no limit at all, the
// "guest-channel-read" bucket included (audit A3, 22 Oct 2026).
//
// When Redis can't be asked, the request is counted in this process instead
// (redisStore.AllowFixedWindowOrLocal). It used to be let through, so a Redis
// outage lifted the sign-in, two-step and reset limits, the ones standing
// between a guesser and a password or a six-digit code.
func IPRateLimit(kind string, max int, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			// A read costs load, not guessing (a link's token is 24 or 32
			// random bytes): its own budget, a minute's.
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if res := redisStore.AllowPerAddress(r.Context(), kind, helpers.ClientIP(r), linkReadsPerAddress); !res.Allowed {
					WriteRateLimited(w, res, registry.PublicReadRate, publicReadAddressMsg)
					return
				}
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

			res := redisStore.AllowFixedWindowOrLocal(rctx, registry.LoginRate, []string{kind, helpers.RateKeyIP(ip)}, max)
			if !res.Allowed {
				WriteRateLimited(w, res, registry.LoginRate, msg)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// WriteRateLimited answers 429 with msg and when to come back: the limiter's
// own wait, else spec's whole window. Every limiter answers through it.
func WriteRateLimited(w http.ResponseWriter, res redisStore.RateLimitResult, spec registry.Spec, msg string) {
	retryAfter := res.RetryAfterSeconds()
	if retryAfter <= 0 {
		retryAfter = int(spec.TTL.Seconds())
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": msg, "status": "rate_limited"})
}

// contextWithRedisTimeout bounds a rate-limit ask of Redis at
// redisStore.LimitAskTimeout.
func contextWithRedisTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, redisStore.LimitAskTimeout)
}
