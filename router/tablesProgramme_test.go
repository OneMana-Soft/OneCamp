package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// A route a tables package registers on Member, on /tables or on /v1 refuses
// someone who isn't signed in (or holds no token), whatever the route itself
// does (tablesProgramme.go). /tables is mounted inside the member group, as
// router.go mounts it.
func TestTablesProgrammeRoutersAreProtected(t *testing.T) {
	root := chi.NewRouter()
	admin, tables, v1 := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	v1.Use(customMiddleware.VerifyApiToken)
	root.Group(func(r chi.Router) {
		r.Use(customMiddleware.CSRFMiddleware, customMiddleware.VerifyAuth)
		r.Mount("/tables", tables)
	})
	root.Mount("/v1", v1)
	rt := newTablesRouters(root, admin, tables, v1)

	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }
	rt.Member.Get("/probe", reached)
	rt.Tables.Get("/probe", reached)
	rt.V1.Get("/probe", reached)

	for _, path := range []string{"/probe", "/tables/probe", "/v1/probe"} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with no credentials answered %d; a programme route must refuse it (401 or 403)", path, rec.Code)
		}
	}
}

// The routers a tables package can reach are pinned: adding one, above all a
// public one, is a decision to take here and in publicSurface_test.go.
func TestTablesProgrammeRoutersArePinned(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(tablesRouters{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	if want := []string{"Member", "Admin", "Tables", "V1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tablesRouters holds %v, want %v", got, want)
	}
}
