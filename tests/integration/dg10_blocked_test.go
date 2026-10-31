//go:build integration

package integration_test

// DG10b: a task's "blocked" count counts only the blockers its reader may
// see. Before DG10 a blocker always sat in its task's project; across
// projects, counting one the reader can't see would say that something they
// can't see exists and is open. Run: integ_rel.sh <wt> <log> 'TestDG10'

import (
	"encoding/json"
	"net/http"
	"testing"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// blockedOpen walks an answer for the task with uuid id and returns its
// task_blocked_open (0 when absent, as the field is omitted at zero), and
// whether the task was found.
func blockedOpen(body, id string) (int, bool) {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return 0, false
	}
	n, found := 0, false
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if x["task_uuid"] == id {
				found = true
				if c, ok := x["task_blocked_open"].(float64); ok && int(c) > n {
					n = int(c)
				}
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
	return n, found
}

// Launch's a waits on Marketing's x and on Launch's b, both open. Ana, in
// both projects, sees a blocked by two; Ben, who can't see Marketing, by one
// (b's); and with b done, a isn't blocked for Ben at all, on the board, the
// list, the timeline and his own tasks.
func TestDG10BlockedCountsOnlyBlockersTheReaderMaySee(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("x", "q", 2, 3, "")
	w.task("b", "p", 2, 3, "")
	w.task("a", "p", 5, 7, "ben", "x", "b")

	reads := map[string]func(who string) (int, string){
		"board": func(who string) (int, string) {
			return w.call(who, false, projectController.GetProjectTaskListForKanban, http.MethodGet, "/", map[string]string{"project_uuid": w.p}, nil)
		},
		"list": func(who string) (int, string) {
			return w.call(who, false, projectController.GetProjectTaskList, http.MethodGet, "/?pageSize=50&pageIndex=0", map[string]string{"project_uuid": w.p}, nil)
		},
		"timeline": func(who string) (int, string) {
			return w.call(who, false, projectController.ProjectTimeline, http.MethodGet, "/", map[string]string{"project_uuid": w.p}, nil)
		},
	}
	check := func(who string, want int) {
		t.Helper()
		for name, read := range reads {
			code, body := read(who)
			got, found := blockedOpen(body, w.tasks["a"])
			if code != http.StatusOK || !found || got != want {
				t.Errorf("%s's %s: a is blocked by %d (found %v, %d), want %d", who, name, got, found, code, want)
			}
		}
	}
	check("ana", 2)
	check("ben", 1)
	// Ben's own tasks: a is assigned to him.
	if code, body := w.call("ben", false, userController.GetDgraphUserTaskList, http.MethodGet, "/?pageSize=50&pageIndex=0", nil, nil); code != http.StatusOK {
		t.Errorf("Ben's tasks: %d %s", code, body)
	} else if got, found := blockedOpen(body, w.tasks["a"]); !found || got != 1 {
		t.Errorf("Ben's tasks: a blocked by %d (found %v), want 1: %s", got, found, body)
	}

	w.dg.Mutate(t, map[string]any{"uid": w.uids["b"], "task_status": "done"})
	check("ana", 1)
	check("ben", 0)
}
