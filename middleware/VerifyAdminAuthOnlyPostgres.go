package middleware

import (
	"net/http"

	authz "github.com/akashc777/OneCamp/business/Authz"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

func VerifyAdminAuthOnlyPostgres(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if !userInfo.UserPostgresInfo.IsAdmin {
			// Says why, as RequireCapability does (QA SH-02).
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"code": AdminRequired,
				"msg":  authz.AdminOnlyMsg,
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}
