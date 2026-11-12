package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A call background made from words is a member's own ask of their own
// model: the route sits behind the session gate, and a caller with no session
// is refused before anything is read.
func TestACallBackgroundIsMadeOnlyForASignedInMember(t *testing.T) {
	t.Setenv("JWT_SECRET", "call-background-route")
	quietLogs()
	var gate string
	for _, r := range walkRoutes(t) {
		if r.method == http.MethodPost && r.pattern == "/ai/call-background" {
			gate = r.gate
		}
	}
	if gate != "session" {
		t.Fatalf("POST /ai/call-background has gate %q, want session (a signed-in member)", gate)
	}
	rec := httptest.NewRecorder()
	routerMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ai/call-background", strings.NewReader(`{"words":"Misty pine forest at dawn"}`)))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("signed out: %d, want 401 or 403 from the session gate", rec.Code)
	}
}
