package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

func TestRequirePlan(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := RequirePlan(helpers.FeatureAuditExport)(ok)

	helpers.SeatLimit = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audit-log/export", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("licensed: status %d, want the handler", rec.Code)
	}

	helpers.SeatLimit = "25"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audit-log/export", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("free plan: status %d, want 403", rec.Code)
	}
}
