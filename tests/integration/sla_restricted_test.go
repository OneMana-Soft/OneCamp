//go:build integration
// +build integration

package integration_test

// A task a member may see only as restricted carries no SLA state on any
// read of the project's work (V-1057): where a link to it stays (what a task
// he reads waits on) it is a stub with no task_sla, on the task's own read,
// the list, the board, and the list filtered and sorted by SLA; Ana, who
// reads it, sees its breach on the same link.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// slaHiddenCounts walks a JSON answer: stubs is how many tasks in it are
// marked hidden, leaks how many of those carry SLA state, and named how many
// tasks named task carry SLA state.
func slaHiddenCounts(t *testing.T, out, task string) (stubs, leaks, named int) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v in %.300s", err, out)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			_, sla := x["task_sla"]
			if x["task_hidden"] == true {
				stubs++
				if sla {
					leaks++
				}
			}
			if x["task_uuid"] == task && sla {
				named++
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return stubs, leaks, named
}

func TestARestrictedTasksStubCarriesNoSLAStateOnAnyRead(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))
	w.onTheirLists("ben")
	if _, err := policyBusiness.Admin.SetIn(context.Background(), policyBusiness.ProjectsSLA.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project},
		json.RawMessage("true"), policyBusiness.Changer{UserID: w.people["ana"], Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
	waits := slaMake(w, "Send the board's reply", "high")
	hidden := slaMake(w, "Answer the board's ticket", "urgent")
	if tm, found := slaTimerOf(w, hidden); !found || tm.state != "running" {
		t.Fatalf("the hidden task's timer: %+v (found %v), want running; events %s", tm, found, slaEvents(w, hidden))
	}
	w.exec(`UPDATE task_slas SET due_at = $2, risk_at = $3 WHERE task_uuid = $1`, hidden, time.Now().Add(-2*time.Hour), time.Now().Add(-8*time.Hour))
	if code, out := slaCall(w, "ana", http.MethodPost, taskController.AddTaskDependency, nil,
		map[string]any{"task_uuid": waits, "blocked_by_uuid": hidden}); code != http.StatusOK {
		t.Fatalf("the reply waits on the ticket: %d %s", code, out)
	}
	if code, out := slaCall(w, "ana", http.MethodPut, taskRestrictionController.SetRestriction, map[string]string{"task_ref": hidden},
		map[string]any{"restricted": true, "people": []string{}}); code != http.StatusOK {
		t.Fatalf("restrict: %d %s", code, out)
	}
	integration.Settle(t)

	list := func(extra url.Values) func(who string) (int, string) {
		return func(who string) (int, string) {
			q := url.Values{"pageSize": {"50"}, "pageIndex": {"0"}}
			for k, v := range extra {
				q[k] = v
			}
			return slaGet(w, who, projectController.GetProjectTaskList, q)
		}
	}
	reads := []struct {
		name string
		read func(who string) (int, string)
		link bool // the link to the hidden task is on this read
	}{
		{"the task that waits on it", func(who string) (int, string) {
			return slaCall(w, who, http.MethodGet, taskController.GetTaskInfo, map[string]string{"task_uuid": waits}, nil)
		}, true},
		{"the list", list(url.Values{"getAll": {"1"}}), false},
		{"the board", func(who string) (int, string) {
			return slaGet(w, who, projectController.GetProjectTaskListForKanban, url.Values{"getAll": {"1"}})
		}, false},
		{"the list filtered by SLA", list(url.Values{"filters": {`[{"id":"task_sla","value":["breached","at_risk","on_track"]}]`}}), false},
		{"the list sorted by SLA", list(url.Values{"sorting": {`[{"id":"task_sla","desc":false}]`}}), false},
	}
	for _, r := range reads {
		code, out := r.read("ana")
		_, _, anaNamed := slaHiddenCounts(t, out, hidden)
		if code != http.StatusOK || (r.link && anaNamed == 0) {
			t.Errorf("%s, as Ana who reads the ticket: %d, %d carry its SLA state; %.300s", r.name, code, anaNamed, out)
		}
		code, out = r.read("ben")
		stubs, leaks, named := slaHiddenCounts(t, out, hidden)
		if code != http.StatusOK || leaks != 0 || named != 0 || (r.link && stubs == 0) {
			t.Errorf("%s, as Ben who may see the ticket only as restricted: %d, %d stubs, %d with SLA state, %d named; %.300s",
				r.name, code, stubs, leaks, named, out)
		}
		t.Logf("%s: Ana %d named with SLA; Ben %d stubs, %d leaks", r.name, anaNamed, stubs, leaks)
	}
}
