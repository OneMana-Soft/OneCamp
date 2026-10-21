package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Connected work mounts its routes, the work graph's among them, on the
// members' and docs' routers alone: nothing on admin or internal, and every
// /graph read is a GET for a signed-in member. Who knows (DG9) is a member's
// alone too: a guest link is not a session and never reaches it.
func TestConnectedWorkMountsItsRoutes(t *testing.T) {
	doc, member, admin, internal := chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	mountConnectedWork(docsRouters{Doc: doc, Member: member, Admin: admin, Internal: internal})
	routes := func(r chi.Router) []string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return out
	}
	want := []string{"GET /graph/connected", "GET /graph/path", "GET /people/who-knows", "GET /people/who-knows/me", "POST /entities", "PUT /people/who-knows/me"}
	if got := routes(member); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("member routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for name, r := range map[string]chi.Router{"admin": admin, "internal": internal} {
		if got := routes(r); len(got) != 0 {
			t.Errorf("%s routes: %v", name, got)
		}
	}
}
