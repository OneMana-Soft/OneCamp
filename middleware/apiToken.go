package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/services"
	"github.com/google/uuid"
)

// apiTokenRateMax is the per-token request cap inside the fixed window
// (registry.ApiTokenRate, 1 minute). Generous enough for normal integration
// traffic, low enough to blunt a runaway script or a leaked token.
const apiTokenRateMax = 120

// uuidNil returns the zero UUID for the active-user sentinel check.
func uuidNil() uuid.UUID { return uuid.UUID{} }

// VerifyApiToken authenticates a request to the public /v1 surface using a
// bearer API token ("Authorization: Bearer oc_..."). On success it injects the
// token owner's UserInfo (so existing business functions run AS that user) plus
// the token's granted scopes, then defers per-route scope checks to
// RequireScope. Failures answer 401 uniformly (no token-existence oracle).
func VerifyApiToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()

		token := bearerToken(r)
		if token == "" {
			unauthorizedJSON(w, "missing or malformed Authorization header")
			return
		}

		auth, err := apiTokenBusiness.Validate(ctx, token)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "middleware/VerifyApiToken validate err: %+v", err)
			unauthorizedJSON(w, "could not validate token")
			return
		}
		if auth == nil {
			unauthorizedJSON(w, "invalid or expired token")
			return
		}

		// Build the owner's UserInfo, mirroring VerifyAuth, so downstream
		// business code sees a normal authenticated user.
		userDBInfo, err := domain.GetActiveUserWithAdminFlagByUserUUID(ctx, auth.UserID)
		emptyUUID := uuidNil()
		// A member's: an external person or a bot never signs in, so a token
		// one holds (minted while it wrongly could) acts for nobody.
		if err != nil || userDBInfo == nil || userDBInfo.Id == emptyUUID || !userDBInfo.IsMember() {
			unauthorizedJSON(w, "token owner is not an active user")
			return
		}
		dgraphUserInfo, err := domain.GetActiveDgraphUserInfoByUUID(ctx, auth.UserID.String())
		if err != nil || dgraphUserInfo == nil {
			unauthorizedJSON(w, "token owner is not an active user")
			return
		}
		dgraphUserInfo.IsAdmin = userDBInfo.IsAdmin

		userInfo := models.UserInfo{
			UserPostgresInfo: *userDBInfo,
			UserDgraphInfo:   *dgraphUserInfo,
		}
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, userInfo)
		ctx = context.WithValue(ctx, helpers.ApiScopesContextKey, auth.Scopes)
		ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, auth.TokenID.String())
		helpers.ServeHidingEmails(next, w, r.WithContext(ctx), userInfo.UserPostgresInfo.EmailID)
	})
}

// ApiTokenRateLimit fixed-window rate-limits the public API per token (req
// 5.2). Must run AFTER VerifyApiToken (which puts the token id in context).
// Fails OPEN when Redis is unavailable so a Redis outage never takes the API
// down. Preflights pass through.
func ApiTokenRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		tokenID := helpers.GetApiTokenID(r.Context())
		if tokenID == "" || !redisStore.IsAvailable() {
			next.ServeHTTP(w, r)
			return
		}
		rctx, cancel := contextWithRedisTimeout(r.Context())
		defer cancel()
		res := redisStore.AllowFixedWindow(rctx, registry.ApiTokenRate, []string{tokenID}, apiTokenRateMax)
		if !res.Allowed {
			retryAfter := res.RetryAfterSeconds()
			if retryAfter <= 0 {
				retryAfter = int(registry.ApiTokenRate.TTL.Seconds())
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			helpers.WriteJSON(w, http.StatusTooManyRequests, services.JsonResponse{
				Error:   true,
				Message: "rate limit exceeded for this token; slow down and retry shortly",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireScope returns middleware asserting the authenticated token carries a
// scope. Use after VerifyApiToken on scoped /v1 routes.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiTokenBusiness.HasScope(helpers.GetApiScopes(r.Context()), scope) {
				next.ServeHTTP(w, r)
				return
			}
			helpers.WriteJSON(w, http.StatusForbidden, services.JsonResponse{
				Error:   true,
				Message: "this token is missing the required scope: " + scope,
			})
		})
	}
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func unauthorizedJSON(w http.ResponseWriter, msg string) {
	helpers.WriteJSON(w, http.StatusUnauthorized, services.JsonResponse{Error: true, Message: msg})
}
