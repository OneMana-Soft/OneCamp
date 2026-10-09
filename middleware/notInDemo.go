package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// Everyone who opens the public demo is the same shared visitor account, so
// whatever one visitor attaches to that account or changes about how it signs
// in, every visitor after them gets, until the nightly reset. These refuse the
// visitor such routes. Anyone else, the demo's own team included, uses them
// as usual, and on any other server they do nothing.

// notForDemoVisitor refuses a route to the demo's shared visitor, saying why.
func notForDemoVisitor(msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if info, ok := r.Context().Value(helpers.UserInfoContextKey).(models.UserInfo); ok &&
				helpers.IsDemoVisitor(info.UserPostgresInfo.EmailID) {
				helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": msg})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NoPersonalAccountsInDemo refuses a route that attaches a person's own
// external account (Gmail, Google Calendar, GitHub) to the shared visitor: an
// account one person connected would be readable by every visitor after them.
var NoPersonalAccountsInDemo = notForDemoVisitor(helpers.DemoPersonalAccountMsg)

// DemoSignInStays refuses the shared visitor a change to how the account signs
// in: a password, two-step sign-in, a passkey. One visitor setting a password
// used to sign every other visitor out (a password change ends the account's
// other sessions), as often as they liked; turning on two-step sign-in would
// leave the next visitor facing a code they don't have.
var DemoSignInStays = notForDemoVisitor(helpers.DemoSignInMsg)
