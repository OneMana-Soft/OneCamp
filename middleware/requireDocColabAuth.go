package middleware

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
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

		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Don't forget to validate the alg is what you expect:
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
			}

			hmacSampleSecret := []byte(os.Getenv("JWT_SECRET"))
			// hmacSampleSecret is a []byte containing your secret, e.g. []byte("my_secret_key")
			return hmacSampleSecret, nil
		})
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if claims, ok := token.Claims.(jwt.MapClaims); ok {

			if float64(time.Now().Unix()) > claims["exp"].(float64) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			userUUID, err := uuid.Parse(claims["sub"].(string))

			if err != nil {
				helpers.LogErrorWithContext(ctx,
					"middleware/VerifyAuth Failed to parse string to uuid err: %+v",
					err)
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
			r = r.WithContext(ctx)

		} else {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
