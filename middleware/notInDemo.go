package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// NoPersonalAccountsInDemo refuses a route that attaches a person's own
// external account (Gmail, Google Calendar, GitHub) to the public demo's
// shared visitor account.
//
// Everyone who opens the demo is that one visitor. An account connected by one
// person would be readable by every visitor after them until the nightly reset,
// so the visitor never connects one. Anyone else, the demo's own team
// included, connects as usual, and on any other server this does nothing.
func NoPersonalAccountsInDemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info, ok := r.Context().Value(helpers.UserInfoContextKey).(models.UserInfo); ok &&
			helpers.IsDemoVisitor(info.UserPostgresInfo.EmailID) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoPersonalAccountMsg})
			return
		}
		next.ServeHTTP(w, r)
	})
}
