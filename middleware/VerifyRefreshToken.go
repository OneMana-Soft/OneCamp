package middleware

import (
	"context"
	"errors"
	"net/http"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

func VerifyRefreshToken(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		tokenString := authcookie.RefreshToken(r)
		if tokenString == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		userUUID, err := helpers.ParseSessionToken(tokenString, helpers.TokenTypeRefresh)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		userDBInfo, err := domain.GetActiveUserByUUID(ctx, userUUID)
		if errors.Is(err, helpers.ErrNotFound) {
			// No one active by that uuid (gone, or deactivated): no session.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"middleware/VerifyRefreshToken Error getting user from postgres err: %+v",
				err)
			// Postgres didn't answer: nothing was refused. A 401 here would
			// sign the device out (the web forgets a session whose refresh is
			// refused), so the refresh is tried again instead (V-1125).
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Members only: a token naming an external person or a bot
		// (models.User.IsMember) gets no new session, however it was minted.
		if !userDBInfo.IsMember() {
			w.WriteHeader(http.StatusUnauthorized)
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
