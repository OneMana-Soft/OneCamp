package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The web app can read Retry-After: a guest page backs off from a 429 or a
// 503 for at least as long as the server asks, and the browser hides any
// header CORS doesn't expose.
func TestTheWebAppCanReadRetryAfter(t *testing.T) {
	const app = "https://app.acme.test"
	h := corsFor([]string{app})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	req := httptest.NewRequest(http.MethodPost, "/guest/channel/tok", nil)
	req.Header.Set("Origin", app)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	exposed := strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers"))
	if !strings.Contains(exposed, "retry-after") || !strings.Contains(exposed, "link") {
		t.Errorf("exposed %q, want Retry-After (and Link)", rec.Header().Get("Access-Control-Expose-Headers"))
	}
}
