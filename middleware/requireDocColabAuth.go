package middleware

import (
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

func VerifyDocColabAuth(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		if tokenString == authHeader {
			// No Bearer prefix
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
		r = r.WithContext(ctx)

		helpers.ServeHidingEmails(next, w, r, userInfo.UserPostgresInfo.EmailID)
	})
}
