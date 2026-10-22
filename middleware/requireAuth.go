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

		emptyUUID := uuid.UUID{}
		userDBInfo, err := domain.GetActiveUserWithAdminFlagByUserUUID(ctx, userUUID)
		if err == nil && (userDBInfo == nil || userDBInfo.Id == emptyUUID) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"middleware/VerifyAuth Error getting user from postgres err: %+v",
				err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Members only: a token naming an external person or a bot
		// (models.User.IsMember) is no session, however it was minted.
		if !userDBInfo.IsMember() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		dgraphUserInfo, err := domain.GetActiveDgraphUserInfoByUUID(ctx, userUUID.String())

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"middleware/VerifyAuth Error getting user from dgraph err: %+v",
				err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		dgraphUserInfo.IsAdmin = userDBInfo.IsAdmin

		userInfo := models.UserInfo{
			UserPostgresInfo: *userDBInfo,
			UserDgraphInfo:   *dgraphUserInfo,
		}
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, userInfo)
		ctx = context.WithValue(ctx, helpers.DeviceIdContextKey, deviceId)
		r = r.WithContext(ctx)

		helpers.ServeHidingEmails(next, w, r, userInfo.UserPostgresInfo.EmailID)
	})
}
