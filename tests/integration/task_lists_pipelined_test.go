//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A2-1 (backend optimisation H4): a task list, a board and My Tasks lay
// their tasks' facts (GitHub's, keys and types, SLA timers, field values,
// formula projects) from reads keyed by the same task ids. Those that don't
// wait on another go to Postgres in one pipelined round trip (pgexec.Batch).
// Measured on beta 6d51aecf3 (16 Nov): a list 9 Postgres round trips, a
// board 12, My Tasks 7, none of them batched. The budgets are what this head
// measures (green-integ2 at decd62f06, 17 Nov) and a ratchet: a rise fails,
// and a drop fails too until the budget is lowered (checkBudget). Lower
// only, never raise.
const (
	listTripsBudget    = 5 // GetProjectTaskList, pageSize 50: GitHub, keys, timers, values, formulas in one
	boardTripsBudget   = 8 // GetProjectTaskListForKanban: the same five in one
	myTasksTripsBudget = 6 // GetDgraphUserTaskListForKanban: keys and timers in one
)

// measuredGet runs h for who with q, the request's ctx marked for calls; the
// count is read once the work the request set going has settled.
func measuredGet(w *workWorld, calls *integration.StoreCalls, who string, h http.HandlerFunc, q url.Values) (int, string) {
	w.t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("project_uuid", w.project)
	account := w.account(who)
	integration.Settle(w.t)
	calls.Reset()
	ctx := calls.Context(context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, account), chi.RouteCtxKey, rc))
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?"+q.Encode(), nil).WithContext(ctx))
	integration.Settle(w.t)
	return rec.Code, rec.Body.String()
}

func callsSaid(calls *integration.StoreCalls) string {
	return fmt.Sprintf("trips %d, statements %d, batches %d, graph %d", calls.Trips(), calls.Statements(), calls.Batches(), calls.Graph())
}

func TestTaskListLaysItsTaskFactsInOneBatch(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, body := measuredGet(w, calls, "ana", projectController.GetProjectTaskList, url.Values{"pageSize": {"50"}, "pageIndex": {"0"}})
	if code != http.StatusOK || !strings.Contains(body, w.task) {
		t.Fatalf("ana's list: %d %.300s", code, body)
	}
	t.Logf("task list: %s", callsSaid(calls))
	if calls.Batches() != 1 {
		t.Errorf("a task list: %s; want its task facts in 1 batch", callsSaid(calls))
	}
	checkBudget(t, calls, "a task list", "listTripsBudget", calls.Trips(), listTripsBudget, "trips")
}

func TestBoardLaysItsTaskFactsInOneBatch(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, body := measuredGet(w, calls, "ana", projectController.GetProjectTaskListForKanban, url.Values{})
	if code != http.StatusOK || !strings.Contains(body, w.task) {
		t.Fatalf("ana's board: %d %.300s", code, body)
	}
	t.Logf("board: %s", callsSaid(calls))
	if calls.Batches() != 1 {
		t.Errorf("a board: %s; want its task facts in 1 batch", callsSaid(calls))
	}
	checkBudget(t, calls, "a board", "boardTripsBudget", calls.Trips(), boardTripsBudget, "trips")
}

func TestMyTasksLaysKeysAndTimersInOneBatch(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, body := measuredGet(w, calls, "ben", userController.GetDgraphUserTaskListForKanban, url.Values{})
	if code != http.StatusOK || !strings.Contains(body, w.mine) {
		t.Fatalf("ben's My Tasks: %d %.300s", code, body)
	}
	t.Logf("My Tasks: %s", callsSaid(calls))
	if calls.Batches() != 1 {
		t.Errorf("My Tasks: %s; want keys and timers in 1 batch", callsSaid(calls))
	}
	checkBudget(t, calls, "My Tasks", "myTasksTripsBudget", calls.Trips(), myTasksTripsBudget, "trips")
}
