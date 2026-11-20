package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// R5a's mount registers its queue beside /project and its decision beside
// /task, each once: nothing on the member root, /admin or the public API.
func TestTriageMountsItsRoutesBesideProjectAndTask(t *testing.T) {
	member, admin, project, task, v1 := chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	mountTriage(projectsRouters{Member: member, Admin: admin, Project: project, Task: task, V1: v1})
	routes := func(r chi.Router) string {
		var out []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		})
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got, want := routes(project), "DELETE /{project_uuid}/triage/mailbox,GET /{project_uuid}/triage,GET /{project_uuid}/triage/mailbox,GET /{project_uuid}/triage/rota,POST /{project_uuid}/triage/mailbox/test,PUT /{project_uuid}/triage/mailbox,PUT /{project_uuid}/triage/rota"; got != want {
		t.Errorf("project routes: %s, want %s", got, want)
	}
	if got, want := routes(task), "POST /triage/{task_uuid}/decide"; got != want {
		t.Errorf("task routes: %s, want %s", got, want)
	}
	for name, r := range map[string]chi.Router{"member": member, "admin": admin, "v1": v1} {
		if got := routes(r); got != "" {
			t.Errorf("%s routes: %s", name, got)
		}
	}
}

// Testing a mailbox dials it (up to 10 s to connect, 30 s a command), so
// POST .../triage/mailbox/test is limited per member: the sixth press inside
// UserWriteRate's window answers 429 before the handler is reached.
func TestTriageMailboxTestIsLimitedPerMember(t *testing.T) {
	if codes := sixPresses(t, http.MethodPost, "/{project_uuid}/triage/mailbox/test", "/triage/mailbox/test"); codes != "[204 204 204 204 204 429]" {
		t.Fatalf("six tests answered %v, want five through and the sixth 429", codes)
	}
}

// A Save has the poller dial the saved host within a minute, so a Save is
// limited as a Test is: the sixth inside the window answers 429, and saving
// can't be made into a way to ask which names resolve where.
func TestTriageMailboxSaveIsLimitedPerMember(t *testing.T) {
	if codes := sixPresses(t, http.MethodPut, "/{project_uuid}/triage/mailbox", "/triage/mailbox"); codes != "[204 204 204 204 204 429]" {
		t.Fatalf("six saves answered %v, want five through and the sixth 429", codes)
	}
}

// sixPresses sends one member's six requests through route's middleware (a
// handler answering 204 behind them), counted in this process as in a Redis
// outage, and returns the codes.
func sixPresses(t *testing.T, method, route, path string) string {
	t.Helper()
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil
	project := chi.NewRouter()
	mountTriage(projectsRouters{Member: chi.NewRouter(), Admin: chi.NewRouter(), Project: project, Task: chi.NewRouter(), V1: chi.NewRouter()})
	var h http.Handler
	_ = chi.Walk(project, func(m, r string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if m == method && r == route {
			h = chi.Chain(mws...).HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		}
		return nil
	})
	if h == nil {
		t.Fatalf("%s %s is not mounted", method, route)
	}
	var who userModels.UserInfo
	who.UserPostgresInfo.Id = uuid.New()
	var codes []int
	for range 6 {
		req := httptest.NewRequest(method, "/"+uuid.NewString()+path, nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	return fmt.Sprint(codes)
}
