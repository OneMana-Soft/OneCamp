package middleware

import (
	"net/http"

	authz "github.com/akashc777/OneCamp/business/Authz"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// AdminOrDemoVisitor lets through a workspace admin, or the public demo's
// shared visitor, and refuses every other member as an admin's route does.
//
// For a route that makes this server ask onemana.dev for something (the
// plan's prices): a server reaches out only when an admin acts, and the demo
// shows its visitor the plan card an admin would see. The visitor is a member,
// not an admin, so an admin's gate alone would keep the demo's card empty.
//
// Must run AFTER VerifyAuth (which puts the person in the context).
func AdminOrDemoVisitor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		info, ok := r.Context().Value(helpers.UserInfoContextKey).(models.UserInfo)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !info.UserPostgresInfo.IsAdmin && !helpers.IsDemoVisitor(info.UserPostgresInfo.EmailID) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": AdminRequired, "msg": authz.AdminOnlyMsg})
			return
		}
		next.ServeHTTP(w, r)
	})
}
