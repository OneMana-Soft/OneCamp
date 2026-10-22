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

func VerifyRefreshTokenForLogout(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		tokenString := authcookie.RefreshToken(r)
		if tokenString == "" {
			// No session of this server's: signing out still clears the cookies.
			next.ServeHTTP(w, r)
			return
		}
		userUUID, err := helpers.ParseSessionToken(tokenString, helpers.TokenTypeRefresh)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		emptyUUID := uuid.UUID{}
		userDBInfo, err := domain.GetUserByUUID(ctx, userUUID)
		if err == nil && userDBInfo.Id == emptyUUID {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			helpers.MessageLogs.ErrorLog.Printf(
				"middleware/VerifyAuth Error getting user from postgres err: %+v",
				err)
			next.ServeHTTP(w, r)
			return
		}

		userInfo := models.UserInfo{
			UserPostgresInfo: *userDBInfo,
		}

		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, userInfo)
		r = r.WithContext(ctx)

		helpers.ServeHidingEmails(next, w, r, userInfo.UserPostgresInfo.EmailID)
	})
}
