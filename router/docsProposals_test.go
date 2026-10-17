package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A9's routes sit on the programme's protected routers: /proposals refuses
// anyone not signed in, /docColab/proposals anyone without the internal secret.
func TestProposalRoutesAreProtected(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "a-test-secret-long-enough-to-count-0123456789")
	root := chi.NewRouter()
	doc, admin, docColab := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	root.Mount("/docColab", docColab)
	mountProposals(newDocsRouters(root, doc, admin, docColab, chi.NewRouter()))

	id := "/7c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/proposals"},
		{http.MethodGet, "/proposals"},
		{http.MethodGet, "/proposals" + id},
		{http.MethodPost, "/proposals" + id + "/decide"},
		{http.MethodPost, "/proposals" + id + "/withdraw"},
		{http.MethodPost, "/proposals" + id + "/reopen"},
		{http.MethodGet, "/docColab/proposals" + id},
		{http.MethodPost, "/docColab/proposals" + id + "/prepared"},
	} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with no credentials answered %d", c.method, c.path, rec.Code)
		}
	}
}
