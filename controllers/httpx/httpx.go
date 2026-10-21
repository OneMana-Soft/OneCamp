// Package httpx is what a handler reads off its request before it does
// anything: who is signed in, and whether they are an admin. Each answers the
// request itself when it can't go on, so a handler reads
//
//	user, ok := httpx.MustUser(w, r)
//	if !ok {
//		return
//	}
//
// rather than asserting the context value (a panic when it is missing, 379
// handlers did) or keeping its own copy (mustUser, requireAdmin, actorFrom).
package httpx

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// UserFrom is the signed-in person the request carries, if any.
func UserFrom(r *http.Request) (*userModel.UserInfo, bool) {
	return userModel.FromContext(r.Context())
}

// MustUser is the signed-in person, answering 401 (and false) when there is
// none. It returns the value the handlers held before, so &user still works.
func MustUser(w http.ResponseWriter, r *http.Request) (userModel.UserInfo, bool) {
	u, ok := UserFrom(r)
	if !ok || u == nil {
		helpers.WriteUnauthorized(w)
		return userModel.UserInfo{}, false
	}
	return *u, true
}

// RequireAdmin is MustUser for a workspace admin: 403 for anyone else.
func RequireAdmin(w http.ResponseWriter, r *http.Request) (userModel.UserInfo, bool) {
	u, ok := MustUser(w, r)
	if !ok {
		return u, false
	}
	if !u.UserPostgresInfo.IsAdmin {
		helpers.LogWarnWithContext(r.Context(), "%s %s refused: %s isn't an admin", r.Method, r.URL.Path, u.UserPostgresInfo.Id)
		helpers.WriteForbidden(w, "Only workspace admins can do that.")
		return userModel.UserInfo{}, false
	}
	return u, true
}
