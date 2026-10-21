package router

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// R1's mount registers every route it promises, each once, beside /project
// only: nothing on the member root, /admin or the public API.
func TestRulesMountTheirRoutesBesideProject(t *testing.T) {
	member, admin, project, task, v1 := chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	mountProjectRules(projectsRouters{Member: member, Admin: admin, Project: project, Task: task, V1: v1})
	routes := func(r chi.Router) string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	want := "GET /{project_uuid}/rules,GET /{project_uuid}/rules/runs,POST /{project_uuid}/rules,POST /{project_uuid}/rules/runs/{run_id}/undo," +
		"POST /{project_uuid}/rules/{rule_id},POST /{project_uuid}/rules/{rule_id}/active,POST /{project_uuid}/rules/{rule_id}/delete"
	if got := routes(project); got != want {
		t.Errorf("project routes:\n%s\nwant\n%s", got, want)
	}
	for name, r := range map[string]chi.Router{"member": member, "admin": admin, "task": task, "v1": v1} {
		if got := routes(r); got != "" {
			t.Errorf("%s routes: %s", name, got)
		}
	}
}
