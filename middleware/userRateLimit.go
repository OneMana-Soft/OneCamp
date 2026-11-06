package middleware

import (
	"net/http"

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
func UserRateLimit(kind string, max int, msg string) func(http.Handler) http.Handler {
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
				WriteRateLimited(w, res, registry.UserWriteRate, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
