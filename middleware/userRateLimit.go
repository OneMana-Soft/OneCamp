package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// UserRateLimit rate-limits a signed-in member's writes per (kind, member) in
// registry.UserWriteRate's 15-minute window, answering 429 with msg once max
// is reached. A route that writes on a member's behalf, such as an export's
// tell to the audit log, is limited by who asks, not where from: IPRateLimit's
// bucket is shared by everyone behind one office's address, so one busy
// member could cost the others their audit rows (V-363). A write no member
// signed in to is counted by its address. Reads and preflights pass.
//
// When Redis can't be asked, the request is counted in this process
// (redisStore.AllowFixedWindowOrLocal), as IPRateLimit does.
//
// held, when not nil, is told before the 429 is written on the first request
// held back in a window: once per (kind, member or address) a window, however
// often the caller keeps pressing, so a record of being held back can't be
// made into a flood. Redis's fixed window counts every request, so that first
// one is the request whose count is just past max (one across every server);
// a window counted in this process (Redis unreachable) tells at most once a
// window in each process. held runs with the request's own context: the
// member is on it.
func UserRateLimit(kind string, max int, msg string, held func(r *http.Request)) func(http.Handler) http.Handler {
	told := &heldOnce{until: map[string]time.Time{}}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodOptions, http.MethodGet, http.MethodHead:
				next.ServeHTTP(w, r)
				return
			}
			who := ""
			if u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok && u.UserPostgresInfo.Id != uuid.Nil {
				who = "member:" + u.UserPostgresInfo.Id.String()
			} else if ip := helpers.ClientIP(r); ip != "" {
				who = helpers.RateKeyIP(ip)
			}
			if who == "" {
				next.ServeHTTP(w, r)
				return
			}

			rctx, cancel := contextWithRedisTimeout(r.Context())
			defer cancel()
			res := redisStore.AllowFixedWindowOrLocal(rctx, registry.UserWriteRate, []string{kind, who}, max)
			if !res.Allowed {
				if held != nil && told.first(who, res, time.Now()) {
					held(r)
				}
				WriteRateLimited(w, res, registry.UserWriteRate, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// heldOnce says whether a held-back request is the first of its window: by
// Redis's count when Redis counted it, else by a note kept in this process
// until the window's end (res.RetryAfter). Notes past their end are dropped as
// new ones are written, so it holds at most the callers held back now.
type heldOnce struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (h *heldOnce) first(who string, res redisStore.RateLimitResult, now time.Time) bool {
	if !res.Unchecked {
		return res.Count == int64(res.Limit)+1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if end, ok := h.until[who]; ok && now.Before(end) {
		return false
	}
	for k, end := range h.until {
		if !now.Before(end) {
			delete(h.until, k)
		}
	}
	h.until[who] = now.Add(res.RetryAfter)
	return true
}
