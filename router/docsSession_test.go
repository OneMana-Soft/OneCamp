package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The session route is the collaboration service's alone: without the shared
// secret it is refused before any token is read.
func TestDocsSessionRouteNeedsTheInternalSecret(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "a-test-secret-long-enough-to-count-0123456789")
	root := chi.NewRouter()
	doc, admin, docColab := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	root.Mount("/docColab", docColab)
	mountDocsSession(newDocsRouters(root, doc, admin, docColab, chi.NewRouter()))

	for _, h := range []map[string]string{{}, {"Authorization": "Bearer x"}} {
		req := httptest.NewRequest(http.MethodPost, "/docColab/session", strings.NewReader(`{"doc_uuid":"d"}`))
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("POST /docColab/session with %v answered %d; want 401 or 403", h, rec.Code)
		}
	}
	// With the secret but no person, the person's token is still required.
	req := httptest.NewRequest(http.MethodPost, "/docColab/session", strings.NewReader(`{"doc_uuid":"d"}`))
	req.Header.Set("X-Internal-Secret", "a-test-secret-long-enough-to-count-0123456789")
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the secret without a bearer token answered %d; want 401", rec.Code)
	}
}
