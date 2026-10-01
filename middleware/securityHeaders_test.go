package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The case that matters is HSTS. Sending it from a plaintext deployment is not a
// cosmetic error: a browser that caches it can no longer reach an http-only
// install, and the operator cannot undo that from the server.

func serve(t *testing.T, r *http.Request, handler http.Handler) http.Header {
	t.Helper()
	if handler == nil {
		handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	}
	rec := httptest.NewRecorder()
	SecurityHeaders(handler).ServeHTTP(rec, r)
	return rec.Result().Header
}

func TestHSTSOnlyWhenTheEdgeWasHTTPS(t *testing.T) {
	cases := []struct {
		name     string
		forwards string
		want     bool
	}{
		{"proxy reports https", "https", true},
		{"proxy reports http", "http", false},
		{"no proxy header at all", "", false},
		{"chained proxies, client half was https", "https, http", true},
		{"chained proxies, client half was http", "http, https", false},
		{"odd casing still counts", "HTTPS", true},
		{"padded value still counts", "  https  ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/health", nil)
			if c.forwards != "" {
				r.Header.Set("X-Forwarded-Proto", c.forwards)
			}
			got := serve(t, r, nil).Get("Strict-Transport-Security") != ""
			if got != c.want {
				t.Errorf("X-Forwarded-Proto=%q: HSTS present = %v, want %v", c.forwards, got, c.want)
			}
		})
	}
}

func TestAlwaysSetsTheHeadersThatCostNothing(t *testing.T) {
	h := serve(t, httptest.NewRequest("GET", "/health", nil), nil)
	for key, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-site",
	} {
		if got := h.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestDoesNotOverruleAHandlerThatChose(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	})
	// The handler runs after the middleware sets its defaults, so the check that
	// matters is the reverse: a value already present before next.ServeHTTP.
	pre := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "origin")
		SecurityHeaders(handler).ServeHTTP(w, r)
	})
	rec := httptest.NewRecorder()
	pre.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if got := rec.Result().Header.Get("Referrer-Policy"); got != "origin" {
		t.Errorf("middleware overruled a deliberate handler value: got %q, want %q", got, "origin")
	}
}
