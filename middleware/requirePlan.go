package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
)

// RequirePlan refuses a route the workspace's plan leaves out (a company
// control on the free plan, see helpers/planFeatures.go) with the same 403 and
// code everywhere, so the web app can show one upgrade prompt for all of them.
func RequirePlan(f helpers.PlanFeature) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !helpers.PlanAllows(f) {
				helpers.WritePlanRequired(w, f)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
