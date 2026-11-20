//go:build integration
// +build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A2-12 (backend optimisation H4): a project's task list reads the project,
// the reader's place in it and the page of tasks in one graph query, and
// decides access from that same answer: someone not in the project is
// refused as before, and sees none of its tasks.
func TestProjectListReadsTheProjectOnce(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	page := url.Values{"pageSize": {"50"}, "pageIndex": {"0"}}
	code, body := measuredGet(w, calls, "ana", projectController.GetProjectTaskList, page)
	if code != http.StatusOK || !strings.Contains(body, w.task) {
		t.Fatalf("ana's list: %d %.300s", code, body)
	}
	if n := calls.Graph(); n != 1 {
		t.Errorf("the list made %d graph calls, want 1 (the project, the reader's place in it and the page together)", n)
	}
}

func TestProjectListRefusesWhoeverIsNotInTheProject(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	page := url.Values{"pageSize": {"50"}, "pageIndex": {"0"}}
	// cy is on the project's team, not in the project; eve has one of its
	// tasks; neither is a member or an admin of it.
	for _, who := range []string{"cy", "eve"} {
		for _, q := range []url.Values{page, {"getAll": {"true"}}, {"pageSize": {"50"}, "pageIndex": {"0"}, "sorting": {`[{"id":"task_name","desc":false}]`}}} {
			code, body := measuredGet(w, calls, who, projectController.GetProjectTaskList, q)
			if code != http.StatusForbidden {
				t.Errorf("%s (%v): %d, want 403", who, q, code)
			}
			for _, task := range []string{w.task, w.assigned, w.mine, "Write the launch post"} {
				if strings.Contains(body, task) {
					t.Errorf("%s (%v) read the project's task %s: %.300s", who, q, task, body)
				}
			}
		}
	}
	// A member reads it, and reads a deleted project's list as before the
	// read was one query: beta 6d51aecf3 answers it (200), the graph's
	// project_deleted_at unread by this route's check.
	if code, _ := measuredGet(w, calls, "ben", projectController.GetProjectTaskList, page); code != http.StatusOK {
		t.Errorf("ben, a member: %d, want 200", code)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": projectUID(w), "project_deleted_at": time.Now().UTC().Format(time.RFC3339)}})
	if code, body := measuredGet(w, calls, "ben", projectController.GetProjectTaskList, page); code != http.StatusOK || !strings.Contains(body, w.task) {
		t.Errorf("a deleted project's list, for a member: %d %.300s, want it as before (200)", code, body)
	}
	if code, body := measuredGet(w, calls, "cy", projectController.GetProjectTaskList, page); code != http.StatusForbidden || strings.Contains(body, w.task) {
		t.Errorf("a deleted project's list, for cy: %d %.300s, want 403", code, body)
	}
}

// projectUID is the world's project's node.
func projectUID(w *workWorld) string {
	w.t.Helper()
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
	}
	raw := w.dg.Query(w.t, `{ p(func: eq(project_uuid, "`+w.project+`")) { uid } }`)
	if err := json.Unmarshal(raw, &got); err != nil || len(got.P) != 1 {
		w.t.Fatalf("the project's node: %+v", got)
	}
	return got.P[0].UID
}
