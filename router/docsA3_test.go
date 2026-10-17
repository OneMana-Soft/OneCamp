package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A3's mounts register every route it promises, each once, on the routers
// the programme hands out (the integrator calls them from mountDocsProgramme).
func TestA3MountsItsRoutes(t *testing.T) {
	doc, member := chi.NewRouter(), chi.NewRouter()
	rt := docsRouters{Doc: doc, Member: member, Admin: chi.NewRouter(), Internal: chi.NewRouter()}
	mountDocTree(rt)
	mountDocLists(rt)
	mountDocTrash(rt)
	mountPlaces(rt)

	routes := func(r chi.Router) []string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return out
	}
	want := []string{
		"GET /ancestors/{doc_uuid}", "GET /children", "GET /list", "GET /trash",
		"POST /createDoc", "POST /delete", "POST /deleteDoc", "POST /duplicate", "POST /move", "POST /move/preview",
		"POST /purge", "POST /restore",
	}
	if got := routes(doc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("/doc routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := routes(member); strings.Join(got, ",") != "DELETE /places,GET /places,POST /places" {
		t.Errorf("member routes: %v", got)
	}
}
