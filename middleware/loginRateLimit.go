package middleware

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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
// unavailable the limiter fails OPEN.
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

// IPRateLimit rate-limits mutating requests per (kind, client IP) in the
// registry.LoginRate 15-minute window, answering 429 with msg once max is
// reached. Fails OPEN when Redis is unavailable.
func IPRateLimit(kind string, max int, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Don't gate preflights or non-mutating probes
			if r.Method == http.MethodOptions || r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}

			if !redisStore.IsAvailable() {
				next.ServeHTTP(w, r)
				return
			}

			ip := clientIP(r)
			if ip == "" {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()

			rctx, cancel := contextWithRedisTimeout(ctx)
			defer cancel()

			res := redisStore.AllowFixedWindow(rctx, registry.LoginRate, []string{kind, ip}, max)
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

// clientIP extracts the originating client IP.
//
// Trust of X-Forwarded-For / X-Real-IP is gated on the TRUST_PROXY_HEADERS
// env var because in dev and self-hosted deployments without a proxy, those
// headers can be spoofed by directly hitting the backend. In production
// behind Traefik / a load-balancer, set TRUST_PROXY_HEADERS=true.
//
// When trusted, only the LEFT-MOST entry of X-Forwarded-For is considered
// (that's the original client per the RFC), and X-Real-IP is the fallback.
func clientIP(r *http.Request) string {
	if strings.EqualFold(os.Getenv("TRUST_PROXY_HEADERS"), "true") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if comma := strings.IndexByte(xff, ','); comma > 0 {
				return strings.TrimSpace(xff[:comma])
			}
			return strings.TrimSpace(xff)
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}

	// RemoteAddr is "host:port"; strip the port. Handles IPv4 and IPv6.
	addr := r.RemoteAddr
	if h, _, err := splitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// splitHostPort is net.SplitHostPort but tolerant of an absent port.
func splitHostPort(addr string) (host, port string, err error) {
	if addr == "" {
		return "", "", fmt.Errorf("empty address")
	}
	// IPv6 literals are bracketed: [::1]:80
	if strings.HasPrefix(addr, "[") {
		if i := strings.LastIndexByte(addr, ']'); i > 0 {
			host = addr[1:i]
			rest := addr[i+1:]
			if strings.HasPrefix(rest, ":") {
				port = rest[1:]
			}
			return host, port, nil
		}
	}
	if i := strings.LastIndexByte(addr, ':'); i > 0 && strings.Count(addr, ":") == 1 {
		return addr[:i], addr[i+1:], nil
	}
	return addr, "", nil
}
