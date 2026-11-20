//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A2-4 (backend optimisation H4): a board drag logs its changes once (status
// and place together), resolves the column once, and what the access check
// read about the task's restriction is carried down, never read again. The
// history a person sees keeps its entries and their order.
// Measured on beta 6d51aecf3 (16 Nov): a drag across columns 25 Postgres
// round trips and 3 graph calls, its changes logged twice and the task's
// restriction read 5 times; a status change 18 and 2, the restriction 4 times.
//
// The budgets were measured on 17 Nov at 1664ad071 (the head's code; the
// commit that sets them changes tests only), three runs, the same each time.
// They are a ratchet: lower only, never raise. The follow-up head H4W (task
// write path) measured them again on 18 Nov at 4cbbf81ba (three runs, the same
// each time): the GitHub link and the cycle schedule come back from the status
// write, and the approvals gate reads a task's open request with its rules in
// one batch. The audit's 12 (status change) / 17 (drag) needs the WorkFeed
// history and its outbox rows written in one statement, which is out of H4W's
// scope (LEAD-16, 18 Nov: the task_changes lock stays its own statement).
const (
	moveTaskBudget     = 16 // Postgres round trips + graph calls, a drag across columns (13 + 3 at H4W; 19 at H4)
	statusChangeBudget = 15 // the same, /updateTaskStatus (13 + 2 at H4W; 18 at H4)
	// restrictionReads is how many statements on these routes read
	// task_restrictions: the access check's own fresh read of the task
	// (accessCheckRead, once; MAIN 17 Nov: the write path's authorisation
	// never takes a cached decision) and the read layer's decision of what
	// the reader may see (models/taskread). That decision stays: a task
	// hidden from the mover must answer as not there before the access check
	// runs, which would answer 403 and so say it exists, and a drag must place
	// the card by the neighbours its mover can see
	// (logs/backend-opt/h4/trace-b3.txt).
	restrictionReads = 2
	accessCheckRead  = "SELECT task_id, project_id, restricted_by"
)

// checkBudget holds what a measured route counted (got, in units: "trips",
// say) to its budget, the constant name: over it fails, and under it too, so
// the budget follows every saving down.
func checkBudget(t *testing.T, calls *integration.StoreCalls, route, name string, got, budget int, units string) {
	t.Helper()
	switch {
	case got > budget:
		t.Errorf("%s: %s; want at most %d %s", route, callsSaid(calls), budget, units)
		saidAll(t, calls)
	case got < budget:
		t.Errorf("%s: %s; fewer than %d %s: lower %s to %d", route, callsSaid(calls), budget, units, name, got)
	}
}

// checkRestrictionReads holds a measured route to the access check's one
// read of the task's restriction, and the read layer's decision beside it.
func checkRestrictionReads(t *testing.T, calls *integration.StoreCalls, route string) {
	t.Helper()
	if n := calls.Sent(accessCheckRead); n != 1 {
		t.Errorf("%s: the access check read the task's restriction %d times, want once", route, n)
	}
	if n := calls.Sent("task_restrictions"); n != restrictionReads {
		t.Errorf("%s: %d statements read task_restrictions, want %d (the access check's and the read layer's decision)", route, n, restrictionReads)
		saidAll(t, calls)
	}
}

// checkNoIdleReads holds a measured route to reading only what its task and
// project have (H4W): no GitHub address for a task that links no issue or
// pull request, and no cycle schedule in a project whose cycles run by hand,
// beyond what the status write itself returns (domain/Task TouchForStatus).
func checkNoIdleReads(t *testing.T, calls *integration.StoreCalls, route string) {
	t.Helper()
	if n := calls.Sent("github_issue_url") - calls.Sent("UPDATE tasks", "github_issue_url"); n != 0 {
		t.Errorf("%s: read the GitHub link of a task that has none %d times, want never", route, n)
	}
	if n := calls.Sent("cycle_schedules") - calls.Sent("UPDATE tasks", "cycle_schedules"); n != 0 {
		t.Errorf("%s: read the cycle schedule of a project that has none %d times, want never", route, n)
	}
}

// measuredPost runs h for who with body, the request's ctx marked for calls.
func measuredPost(w *workWorld, calls *integration.StoreCalls, who string, h http.HandlerFunc, body any) (int, string) {
	w.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	integration.Settle(w.t)
	calls.Reset()
	code, out := servePost(w, calls, who, h, raw)
	integration.Settle(w.t)
	return code, out
}

// servePost runs h for who with the JSON raw and returns as soon as h does,
// not waiting for the work it hands off.
func servePost(w *workWorld, calls *integration.StoreCalls, who string, h http.HandlerFunc, raw []byte) (int, string) {
	ctx := calls.Context(context.WithValue(context.Background(), helpers.UserInfoContextKey, w.account(who)))
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// changesOf is the task's history in the work feed, oldest first: which
// fields each entry changed, and who changed them.
func changesOf(w *workWorld, task string) []string {
	w.t.Helper()
	rows, err := w.env.PG.Query(`SELECT diff::text, coalesce(actor_uuid::text, '') FROM task_changes WHERE resource_uuid = $1 ORDER BY sequence`, task)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var diff, actor string
		if err := rows.Scan(&diff, &actor); err != nil {
			w.t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(diff), &fields)
		kind := "other"
		switch {
		case fields["task_status"] != nil:
			kind = "status"
		case fields["task_rank"] != nil:
			kind = "rank"
		}
		who := "nobody"
		for name, id := range w.people {
			if id.String() == actor {
				who = name
			}
		}
		out = append(out, kind+" by "+who)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func TestBoardDragLogsOnceAndReadsTheRestrictionOnce(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, out := measuredPost(w, calls, "ana", taskController.MoveTask,
		map[string]string{"task_uuid": w.task, "task_status": "inProgress", "before_task_uuid": "", "after_task_uuid": ""})
	if code != http.StatusOK {
		t.Fatalf("the drag: %d %s", code, out)
	}
	t.Logf("moveTask: %s", callsSaid(calls))
	if n := calls.Sent("INSERT INTO task_changes"); n != 1 {
		t.Errorf("the drag logged its changes %d times, want once", n)
	}
	checkRestrictionReads(t, calls, "the drag")
	checkNoIdleReads(t, calls, "the drag")
	checkBudget(t, calls, "moveTask", "moveTaskBudget", calls.Trips()+calls.Graph(), moveTaskBudget, "trips and graph calls")
}

func TestStatusChangeReadsTheRestrictionOnce(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, out := measuredPost(w, calls, "ana", taskController.UpdateTaskStatus, map[string]string{"task_uuid": w.task, "task_status": "inProgress", "task_project_uuid": w.project})
	if code != http.StatusOK {
		t.Fatalf("the status change: %d %s", code, out)
	}
	t.Logf("status change: %s", callsSaid(calls))
	checkRestrictionReads(t, calls, "the status change")
	checkNoIdleReads(t, calls, "the status change")
	checkBudget(t, calls, "a status change", "statusChangeBudget", calls.Trips()+calls.Graph(), statusChangeBudget, "trips and graph calls")
}

// The history a drag leaves is what it always was: across columns its status
// then its place, within a column its place only, and a status change alone
// its status only, each by whoever moved it.
func TestBoardDragKeepsItsHistory(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	for _, step := range []struct {
		name string
		h    http.HandlerFunc
		body map[string]string
		want []string
	}{
		{"across columns", taskController.MoveTask,
			map[string]string{"task_uuid": w.task, "task_status": "inProgress", "after_task_uuid": ""},
			[]string{"status by ana", "rank by ana"}},
		{"within its column", taskController.MoveTask,
			map[string]string{"task_uuid": w.task, "task_status": "inProgress", "after_task_uuid": ""},
			[]string{"status by ana", "rank by ana", "rank by ana"}},
		{"its status only", taskController.UpdateTaskStatus,
			map[string]string{"task_uuid": w.task, "task_status": "done", "task_project_uuid": w.project},
			[]string{"status by ana", "rank by ana", "rank by ana", "status by ana"}},
	} {
		if code, out := measuredPost(w, calls, "ana", step.h, step.body); code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.name, code, out)
		}
		t.Logf("%s: %s", step.name, callsSaid(calls))
		if got := changesOf(w, w.task); strings.Join(got, ", ") != strings.Join(step.want, ", ") {
			t.Errorf("%s: the history is %v, want %v", step.name, got, step.want)
		}
	}
}

// saidAll lists the statements a measured call sent, for a budget it broke.
func saidAll(t *testing.T, calls *integration.StoreCalls) {
	t.Helper()
	for i, s := range calls.Said() {
		t.Logf("  %2d %s", i+1, s)
	}
}
