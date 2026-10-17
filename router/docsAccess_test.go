package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A5's mount registers every route it promises, each once, on the routers the
// programme hands out (the integrator calls it from mountDocsProgramme).
func TestA5MountsItsRoutes(t *testing.T) {
	doc, admin := chi.NewRouter(), chi.NewRouter()
	rt := docsRouters{Doc: doc, Member: chi.NewRouter(), Admin: admin, Internal: chi.NewRouter()}
	mountDocAccess(rt)

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
		"GET /access/requests", "GET /access/{doc_uuid}", "GET /getDocPermissions",
		"POST /access/answer", "POST /access/preview", "POST /access/request", "POST /access/transfer",
		"POST /access/update", "POST /lock", "POST /removeComment", "POST /updateDocPermissions",
	}
	if got := routes(doc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("/doc routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := routes(admin); strings.Join(got, ",") != "POST /docs/recover" {
		t.Errorf("admin routes: %v", got)
	}
}
