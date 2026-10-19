package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A1's mount registers every route it promises, each once, on the routers
// the programme hands out: roles and the project's policies beside /project,
// the steward step behind /admin, the notices behind the member group.
func TestA1MountsItsRoutes(t *testing.T) {
	member, admin, project := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	mountProjectRoles(projectsRouters{Member: member, Admin: admin, Project: project, Task: chi.NewRouter(), V1: chi.NewRouter()})

	routes := func(r chi.Router) string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got, want := routes(project), "GET /{project_uuid}/policies,POST /{project_uuid}/members/{user_uuid}/role,PUT /{project_uuid}/policies/{key}"; got != want {
		t.Errorf("project routes: %s, want %s", got, want)
	}
	if got, want := routes(admin), "POST /teams/{team_uuid}/admins/me"; got != want {
		t.Errorf("admin routes: %s, want %s", got, want)
	}
	if got, want := routes(member), "GET /notices,POST /notices/{key}/seen"; got != want {
		t.Errorf("member routes: %s, want %s", got, want)
	}
}
