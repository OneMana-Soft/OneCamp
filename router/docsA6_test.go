package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A6's mounts register every route it promises, each once, on the routers
// the programme hands out (the integrator calls them from mountDocsProgramme).
func TestA6MountsItsRoutes(t *testing.T) {
	member, admin := chi.NewRouter(), chi.NewRouter()
	rt := docsRouters{Doc: chi.NewRouter(), Member: member, Admin: admin, Internal: chi.NewRouter()}
	mountFollows(rt)
	mountPolicies(rt)

	routes := func(r chi.Router) string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got := routes(member); got != "DELETE /follows,GET /follows,PUT /follows" {
		t.Errorf("member routes: %s", got)
	}
	if got := routes(admin); got != "GET /policies,PUT /policies/{key}" {
		t.Errorf("admin routes: %s", got)
	}
}
