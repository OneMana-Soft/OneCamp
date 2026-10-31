package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// corsAnswers is whether credentialed CORS answers a page's read from origin,
// with the router's origins as the environment sets them now.
func corsAnswers(origin string) bool {
	r := chi.NewRouter()
	useBaseMiddleware(r, allowedOrigins())
	r.Get("/channel/list", func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest(http.MethodOptions, "/channel/list", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Header().Get("Access-Control-Allow-Origin") == origin
}

// Audit, LOW: the API answered credentialed CORS for the web's dev servers on
// every install, so a page on a visitor's own localhost:3000 could read
// answers from a production API as them. Only a development backend, where
// cookies are insecure, lets next dev in now.
func TestCORSLetsTheWebsDevServersInOnlyOnADevelopmentBackend(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	if !corsAnswers("https://acme.onemana.dev") {
		t.Fatal("the web app's own origin was refused")
	}
	for _, origin := range []string{"http://localhost:3000", "https://localhost:3000", "http://localhost:3001", "https://localhost:3001"} {
		if corsAnswers(origin) {
			t.Errorf("%s was answered by an https install", origin)
		}
	}
	t.Setenv("COOKIE_SECURE", "false")
	if !corsAnswers("http://localhost:3000") {
		t.Error("next dev was refused by a development backend")
	}
}

// CODE-REVIEWER-15, 23 Oct: CORS answered plain http for any host named
// localhost, even where cookies are secure, while the CSRF check allowed it
// only where they are insecure. Both now start from middleware.OwnOrigins.
func TestCORSAnswersItsOwnHostOverPlainHTTPOnlyOnADevelopmentBackend(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "localhost:8443")
	t.Setenv("BACKEND_DOMAIN", "")
	t.Setenv("COOKIE_SECURE", "")
	if !corsAnswers("https://localhost:8443") {
		t.Fatal("the web app's own origin was refused")
	}
	if corsAnswers("http://localhost:8443") {
		t.Error("an https install answered its own host over plain http")
	}
	t.Setenv("COOKIE_SECURE", "false")
	if !corsAnswers("http://localhost:8443") {
		t.Error("a development backend refused its own host over plain http")
	}
}
