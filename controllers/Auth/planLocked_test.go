package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

func TestPlanLockedSignIn(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })

	helpers.SeatLimit = ""
	if planLocked(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/saml/login", nil), helpers.FeatureSSO, true) {
		t.Fatal("a licensed workspace must reach its SSO")
	}

	helpers.SeatLimit = "25"
	// A browser lands back on the sign-in page with the reason, like a full seat plan.
	rec := httptest.NewRecorder()
	if !planLocked(rec, httptest.NewRequest(http.MethodGet, "/saml/login", nil), helpers.FeatureSSO, true) {
		t.Fatal("the free plan must refuse SSO")
	}
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.Contains(loc, "error=plan_required") || !strings.Contains(loc, "message=") {
		t.Fatalf("browser refusal: %d %q", rec.Code, loc)
	}
	// An API caller (LDAP posts JSON) gets the 403.
	rec = httptest.NewRecorder()
	planLocked(rec, httptest.NewRequest(http.MethodPost, "/auth/ldap-login", nil), helpers.FeatureLDAP, false)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"plan_required"`) {
		t.Fatalf("api refusal: %d %s", rec.Code, rec.Body.String())
	}
}
