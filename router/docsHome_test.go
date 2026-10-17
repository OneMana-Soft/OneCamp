package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// B5's mount registers the docs home's routes, each once, on /doc only.
func TestDocsHomeMountsItsRoutes(t *testing.T) {
	doc, member := chi.NewRouter(), chi.NewRouter()
	rt := docsRouters{Doc: doc, Member: member, Admin: chi.NewRouter(), Internal: chi.NewRouter()}
	mountDocsHome(rt)
	var got []string
	_ = chi.Walk(doc, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	})
	sort.Strings(got)
	want := "GET /search,POST /applyTemplate,POST /saveAsTemplate"
	if strings.Join(got, ",") != want {
		t.Errorf("/doc routes %v, want %s", got, want)
	}
	_ = chi.Walk(member, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		t.Errorf("nothing goes on the member router, got %s %s", method, route)
		return nil
	})
}
