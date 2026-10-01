package middleware

// Authentication for the SCIM surface.
//
// A SEPARATE MIDDLEWARE FROM VerifyApiToken, for a reason that is not symmetry. VerifyApiToken resolves
// the token's OWNER and refuses the request when that person is not an active user — correct for /v1,
// where every call runs as somebody and must not exceed their permissions. SCIM is the opposite
// situation: it does not act as a person, it acts on people, and its most important operation is
// deactivating them. Borrowing the owner check would mean the directory loses access the moment it
// offboards whoever configured it, so provisioning stops working precisely when it is being used
// correctly.
//
// So this authenticates a WORKSPACE credential and injects no user identity at all. The SCIM handlers
// therefore cannot read a UserInfo from context, which is deliberate: there is no acting member, and a
// handler that quietly ran as one would grant a directory whatever that member could reach.

import (
	"context"
	"net/http"

	scimBusiness "github.com/akashc777/OneCamp/business/Scim"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services"
)

// scimTokenIDKey carries the authenticated credential's id for logging.
//
// Its own unexported type so it cannot collide with another package's context key — the documented
// hazard of using a bare string.
type scimTokenIDKeyType struct{}

var scimTokenIDKey = scimTokenIDKeyType{}

// VerifyScimToken authenticates a SCIM request by bearer credential.
//
// Named to be recognised by router/publicSurface_test.go, whose subRouterAuth pattern lists the
// middlewares that may legitimately protect a sub-router mounted outside the cookie-auth group. That
// list is a closed set on purpose: a new name has to be added to it consciously, which is the moment to
// check that the thing being added really does authenticate.
func VerifyScimToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()

		token := bearerToken(r)
		if token == "" {
			scimUnauthorized(w, "a bearer credential is required")
			return
		}

		row, err := scimBusiness.ValidateScimToken(ctx, token)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "middleware/VerifyScimToken validate err: %+v", err)
			// A lookup failure is not an invalid credential, and answering 401 would tell a correctly
			// configured directory to stop trying. 503 says "ask again", which is what it should do.
			helpers.WriteJSON(w, http.StatusServiceUnavailable, services.JsonResponse{
				Error:   true,
				Message: "could not verify the credential; retry shortly",
			})
			return
		}
		if row == nil {
			// One answer for missing, unknown, revoked and expired, so the response cannot be used to
			// discover which credentials exist.
			scimUnauthorized(w, "invalid or expired credential")
			return
		}

		// Fire-and-forget on a context that outlives the request: this is the only signal an operator has
		// that a directory connection is alive, and it must not be cancelled by the response being
		// written. Using r.Context() here would make the update lose a race with the client disconnecting.
		go scimBusiness.TouchScimToken(context.WithoutCancel(ctx), row.Id)

		ctx = context.WithValue(ctx, scimTokenIDKey, row.Id.String())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ScimTokenIDFromContext returns the authenticated credential's id, or "".
//
// Exposed so handlers can attribute a provisioning action to the credential that made it. There is no
// user to attribute it to, which is the point of the surface, so the credential is the only actor there
// is to name in a log.
func ScimTokenIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(scimTokenIDKey).(string)
	return id
}

// scimUnauthorized answers 401 with a WWW-Authenticate challenge.
//
// The header is what makes the failure diagnosable in an IdP's admin console: without it, a client
// reports "401" and an operator cannot tell a rejected credential from a route that wanted a session
// cookie. It is also what RFC 7235 requires of a 401.
func scimUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
	helpers.WriteJSON(w, http.StatusUnauthorized, services.JsonResponse{Error: true, Message: msg})
}
