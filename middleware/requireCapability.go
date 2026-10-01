package middleware

import (
	"net/http"

	authz "github.com/akashc777/OneCamp/business/Authz"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// RequireCapability returns middleware that allows a request through only if
// the authenticated user may exercise the given capability (admins always may;
// members may when an admin has opened the capability to all members). This is
// the generic, reusable gate for delegatable features — mount it on a route
// group instead of hard-coding an admin-only check.
//
// Must run AFTER VerifyAuth (which populates UserInfo in the context).
func RequireCapability(capability string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !authz.Can(ctx, &userInfo, capability) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
