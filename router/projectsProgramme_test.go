package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A route a projects package registers on Member refuses someone who isn't
// signed in, whatever the route itself does (projectsProgramme.go).
func TestProjectsProgrammeRoutersAreProtected(t *testing.T) {
	root := chi.NewRouter()
	r := func() chi.Router { return chi.NewRouter() }
	rt := newProjectsRouters(root, r(), r(), r(), r())
	rt.Member.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("GET /probe with no credentials answered %d; a programme route must refuse it", rec.Code)
	}
}

// The routers a projects package can reach are pinned: adding one, above all
// the root, is a decision to take here and in publicSurface_test.go.
func TestProjectsProgrammeRoutersArePinned(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(projectsRouters{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	if want := []string{"Member", "Admin", "Project", "Task", "V1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("projectsRouters holds %v, want %v", got, want)
	}
}
