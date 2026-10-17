package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// B6's mount registers every route it promises, each once, on /doc alone.
func TestB6MountsItsRoutes(t *testing.T) {
	doc, member, admin, internal := chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	mountDocChrome(docsRouters{Doc: doc, Member: member, Admin: admin, Internal: internal})

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
		"GET /backlinks/{doc_uuid}", "GET /getDocInfo/{doc_uuid}", "GET /trashed/{doc_uuid}",
		"POST /access/invite", "POST /look",
	}
	if got := routes(doc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("/doc routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for name, r := range map[string]chi.Router{"member": member, "admin": admin, "internal": internal} {
		if got := routes(r); len(got) != 0 {
			t.Errorf("%s routes: %v", name, got)
		}
	}
}
