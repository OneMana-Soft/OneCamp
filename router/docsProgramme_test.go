package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// Every router the docs programme hands a package is protected (docsProgramme.go).
// A route a package registers on Member must refuse someone who is not signed in,
// and one on Internal must refuse anyone without the internal secret, whatever
// the route itself does.
func TestDocsProgrammeRoutersAreProtected(t *testing.T) {
	// A secret the middleware accepts as set, so the refusal is for the missing
	// header and not for a server with no secret configured.
	t.Setenv("INTERNAL_SECRET", "a-test-secret-long-enough-to-count-0123456789")

	root := chi.NewRouter()
	doc, admin, docColab, v1 := chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	v1.Use(customMiddleware.VerifyApiToken)
	root.Mount("/docColab", docColab)
	root.Mount("/v1", v1)
	rt := newDocsRouters(root, doc, admin, docColab, v1)

	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }
	rt.Member.Get("/probe", reached)
	rt.Internal.Get("/probe", reached)
	rt.V1.Get("/probe", reached)
	rt.Files.Get("/files-probe", reached)

	for _, path := range []string{"/probe", "/docColab/probe", "/v1/probe", "/files-probe"} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with no credentials answered %d; a programme route must refuse it (401 or 403)", path, rec.Code)
		}
	}
}

// The routers a package can reach are pinned. Adding one to docsRouters, above
// all an unprotected root router, is a decision to take here and in
// publicSurface_test.go, not a convenience.
func TestDocsProgrammeRoutersArePinned(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(docsRouters{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	want := []string{"Doc", "Member", "Admin", "Internal", "V1", "Files"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("docsRouters holds %v, want %v", got, want)
	}
}
