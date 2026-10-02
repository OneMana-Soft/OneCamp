package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
)

// NoPersonalAccountsInDemo refuses a route that attaches a person's own
// external account (Gmail, Google Calendar, GitHub) on the public demo.
//
// Everyone on the demo is the same visitor. An account connected by one person
// would be readable by every visitor after them until the nightly reset, so the
// demo never connects one. Elsewhere it does nothing.
func NoPersonalAccountsInDemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if helpers.DemoMode() {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoPersonalAccountMsg})
			return
		}
		next.ServeHTTP(w, r)
	})
}
