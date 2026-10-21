package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A route a calls package registers on Member refuses someone who isn't
// signed in, whatever the route itself does (callsProgramme.go).
func TestCallsProgrammeRoutersAreProtected(t *testing.T) {
	root := chi.NewRouter()
	rt := newCallsRouters(root)
	rt.Member.Get("/probe", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("GET /probe with no credentials answered %d; a programme route must refuse it", rec.Code)
	}
}

// The routers a calls package can reach are pinned: adding one, above all a
// public one, is a decision to take here and in publicSurface_test.go.
func TestCallsProgrammeRoutersArePinned(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(callsRouters{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	if want := []string{"Member"}; !reflect.DeepEqual(got, want) {
		t.Errorf("callsRouters holds %v, want %v", got, want)
	}
}
