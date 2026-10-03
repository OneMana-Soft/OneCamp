package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

func TestSCIMRefusedOnTheFreePlanInSCIMsOwnFormat(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	reached := false
	h := RequireSCIMPlan(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	helpers.SeatLimit = "25"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil))
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("free plan: reached=%v status=%d", reached, rec.Code)
	}
	body := rec.Body.String()
	// Okta and Entra show SCIM's own error detail to whoever set the connection up.
	if !strings.Contains(body, "urn:ietf:params:scim:api:messages:2.0:Error") || !strings.Contains(body, "SCIM provisioning needs a OneCamp licence") {
		t.Fatalf("body %s", body)
	}

	helpers.SeatLimit = ""
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil))
	if !reached {
		t.Fatal("a licensed workspace must reach SCIM")
	}
}
