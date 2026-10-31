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

// The web app reads its CSRF token from the X-CSRF-Token header of the API's
// answers (the cookie that holds it is the API host's own and HttpOnly), so
// CORS must expose it.
func TestTheWebAppCanReadItsCSRFToken(t *testing.T) {
	const app = "https://acme.onemana.dev"
	h := corsFor([]string{app})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-CSRF-Token", "token")
	}))
	req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
	req.Header.Set("Origin", app)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if exposed := strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers")); !strings.Contains(exposed, "x-csrf-token") {
		t.Errorf("exposed %q, want X-CSRF-Token", rec.Header().Get("Access-Control-Expose-Headers"))
	}
}

// The web app sends an import with an Idempotency-Key (docs W3), so a
// preflight must allow it: a header CORS doesn't allow stops the request in
// the browser, and the import with it.
func TestTheWebAppMaySendAnIdempotencyKey(t *testing.T) {
	const app = "https://acme.onemana.dev"
	h := corsFor([]string{app})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/doc/import", nil)
	req.Header.Set("Origin", app)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type,idempotency-key,x-csrf-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if allowed := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(allowed, "idempotency-key") {
		t.Errorf("a preflight allowed %q, want Idempotency-Key", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

// The web app reads a download's name from Content-Disposition, and a task
// list CSV's counts from X-Total-Count and X-Export-Rows, so it can say when
// the file holds only the first part of the list. Before, the browser hid the
// name, and every time report saved as "time.csv".
func TestTheWebAppCanReadADownloadsNameAndCounts(t *testing.T) {
	const app = "https://acme.onemana.dev"
	h := corsFor([]string{app})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Launch tasks.csv"`)
		w.Header().Set("X-Total-Count", "63201")
		w.Header().Set("X-Export-Rows", "50000")
	}))
	req := httptest.NewRequest(http.MethodGet, "/project/taskList/p?format=csv", nil)
	req.Header.Set("Origin", app)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	exposed := strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers"))
	for _, want := range []string{"content-disposition", "x-total-count", "x-export-rows"} {
		if !strings.Contains(exposed, want) {
			t.Errorf("exposed %q, want %s", rec.Header().Get("Access-Control-Expose-Headers"), want)
		}
	}
}

// An admin's change to what an app may do names the version it was read at in
// If-Match (PUT /admin/apps/{id}/access), and the web app calls the API from its
// own origin: the browser asks first, and a header CORS doesn't allow stops
// the PUT before it is sent.
func TestTheWebAppCanSendIfMatch(t *testing.T) {
	const app = "https://acme.onemana.dev"
	h := corsFor([]string{app})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/admin/apps/a1/access", nil)
	req.Header.Set("Origin", app)
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	req.Header.Set("Access-Control-Request-Headers", "content-type, if-match")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	origin, allowed := rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("Access-Control-Allow-Headers")
	if origin != app || !strings.Contains(strings.ToLower(allowed), "if-match") {
		t.Errorf("a preflight asking for If-Match was answered with origin %q and headers %q, want the app's origin and If-Match allowed", origin, allowed)
	}
}
