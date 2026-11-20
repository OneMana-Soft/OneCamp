package middleware

import (
	"context"
	"net/http"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func VerifyAuth(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		// This host's own cookies, or this server's from before they moved
		// (authcookie): never another workspace's sent along on a shared zone.
		tokenString := authcookie.AccessToken(r)
		deviceId := authcookie.DeviceID(r)
		if tokenString == "" || deviceId == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		userUUID, err := helpers.ParseSessionToken(tokenString, helpers.TokenTypeAccess)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		ctx, userInfo, ok := memberSession(ctx, userUUID)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ctx = context.WithValue(ctx, helpers.DeviceIdContextKey, deviceId)
		r = r.WithContext(ctx)

		helpers.ServeHidingEmails(next, w, r, userInfo.UserPostgresInfo.EmailID)
	})
}

// memberSession is who the session's member is, and ctx carrying it
// (helpers.UserInfoContextKey) and the task-read generation: both profiles
// are read with the generation in one round trip (domain.MemberSession).
// false, for a 401, when userUUID names no live member or a read failed.
func memberSession(ctx context.Context, userUUID uuid.UUID) (context.Context, models.UserInfo, bool) {
	ctx, user, dgraphUser, err := domain.MemberSession(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "middleware: reading the session's profiles err: %+v", err)
	}
	if err != nil || user == nil {
		return ctx, models.UserInfo{}, false
	}
	dgraphUser.IsAdmin = user.IsAdmin
	info := models.UserInfo{UserPostgresInfo: *user, UserDgraphInfo: *dgraphUser}
	return context.WithValue(ctx, helpers.UserInfoContextKey, info), info, true
}
