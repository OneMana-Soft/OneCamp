package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every thread route refuses someone who is not signed in, whatever its
// handler would do (they all sit on the programme's member router).
func TestThreadRoutesRefuseTheSignedOut(t *testing.T) {
	root := chi.NewRouter()
	doc, admin, docColab := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	root.Mount("/docColab", docColab)
	mountThreads(newDocsRouters(root, doc, admin, docColab, chi.NewRouter()))

	id := "/threads/7a1d1c2e-0000-4000-8000-000000000001"
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/threads?resource_kind=doc&resource_uuid=x"},
		{http.MethodPost, "/threads"},
		{http.MethodGet, id},
		{http.MethodPost, id + "/replies"},
		{http.MethodPost, id + "/resolve"},
		{http.MethodPost, id + "/reopen"},
		{http.MethodPost, id + "/reanchor"},
		{http.MethodPost, id + "/read"},
		{http.MethodPost, id + "/comments/7a1d1c2e-0000-4000-8000-000000000002/edit"},
		{http.MethodPost, id + "/comments/7a1d1c2e-0000-4000-8000-000000000002/remove"},
		{http.MethodPost, id + "/comments/7a1d1c2e-0000-4000-8000-000000000002/react"},
		{http.MethodPost, id + "/comments/7a1d1c2e-0000-4000-8000-000000000002/unreact"},
	} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s signed out answered %d", c.method, c.path, rec.Code)
		}
	}
}
