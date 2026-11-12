//go:build integration

package integration_test

// CAL51-TB: the calendar's Tasks list and its blocks' dependencies. The
// list is the caller's own open tasks across their projects (open=1), dated
// or not and in a project's own statuses too, each with its estimate; a
// block's task says how many open tasks it waits on in the reader's projects
// and names the first only to a reader who may read it. One graph read for a
// week of blocks.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCAL51OpenTasksAndWhatABlockWaitsOn(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("x", "q", 2, 3, "")
	w.task("b", "p", 2, 3, "")
	w.task("a", "p", 5, 7, "ben", "x", "b")
	w.taskAt("undated", "p", time.Time{}, time.Time{}, map[string]any{"task_estimate_minutes": 90}, "ben")
	w.taskAt("own", "p", time.Time{}, time.Time{}, map[string]any{"task_status": "in_progress", "task_custom_status": "5b0e6a0c-1c4b-4d1e-9a51-3a1d2b6c7e80"}, "ben")
	w.taskAt("finished", "p", time.Time{}, dg10Day(4, 17), map[string]any{"task_status": "done"}, "ben")
	w.taskAt("dropped", "p", time.Time{}, time.Time{}, map[string]any{"task_status": "canceled"}, "ben")

	code, body := w.call("ben", false, userController.GetDgraphUserTaskList, http.MethodGet, "/?pageSize=50&pageIndex=0&open=1", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("Ben's open tasks: %d %s", code, body)
	}
	var res struct {
		Data struct {
			Tasks []struct {
				UUID     string `json:"task_uuid"`
				Estimate int    `json:"task_estimate_minutes"`
				Blocked  int    `json:"task_blocked_open"`
			} `json:"user_tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, task := range res.Data.Tasks {
		got[task.UUID] = task.Estimate
	}
	for _, name := range []string{"a", "undated", "own"} {
		if _, ok := got[w.tasks[name]]; !ok {
			t.Errorf("open=1 leaves out %s: %s", name, body)
		}
	}
	for _, name := range []string{"finished", "dropped"} {
		if _, ok := got[w.tasks[name]]; ok {
			t.Errorf("open=1 keeps %s", name)
		}
	}
	if got[w.tasks["undated"]] != 90 {
		t.Errorf("the list carries the estimate: %d", got[w.tasks["undated"]])
	}

	// What a block's task waits on: Ben is in Launch alone, so Marketing's x
	// isn't there for him, not even as a number; Ana counts both.
	ctx := context.Background()
	ben, err := taskBusiness.ReadableBlockTasks(ctx, []string{w.tasks["a"]}, w.uids["ben"], w.people["ben"].String())
	if err != nil {
		t.Fatal(err)
	}
	if a := ben[w.tasks["a"]]; a.BlockedOpen != 1 || a.FirstBlocker == nil || a.FirstBlocker.UUID != w.tasks["b"] || a.FirstBlocker.Name != "b" {
		t.Errorf("Ben's block: %+v %+v", a, a.FirstBlocker)
	}
	ana, err := taskBusiness.ReadableBlockTasks(ctx, []string{w.tasks["a"]}, w.uids["ana"], w.people["ana"].String())
	if err != nil {
		t.Fatal(err)
	}
	if a := ana[w.tasks["a"]]; a.BlockedOpen != 2 || a.FirstBlocker == nil || a.FirstBlocker.Name == "" {
		t.Errorf("Ana's block: %+v %+v", a, a.FirstBlocker)
	}
	// A blocker that's done no longer counts.
	w.dg.Mutate(t, map[string]any{"uid": w.uids["b"], "task_status": "done"})
	ben, _ = taskBusiness.ReadableBlockTasks(ctx, []string{w.tasks["a"]}, w.uids["ben"], w.people["ben"].String())
	if a := ben[w.tasks["a"]]; a.BlockedOpen != 0 || a.FirstBlocker != nil {
		t.Errorf("Ben's block once b is done: %+v", a)
	}

	// A week of 50 blocks, each waiting on one open task: one read.
	w.task("gate", "p", 2, 3, "")
	ids := make([]string, 0, 50)
	for i := range 50 {
		name := fmt.Sprintf("w%02d", i)
		w.task(name, "p", 9, 10, "ben", "gate")
		ids = append(ids, w.tasks[name])
	}
	var took time.Duration
	for range 3 {
		began := time.Now()
		week, err := taskBusiness.ReadableBlockTasks(ctx, ids, w.uids["ben"], w.people["ben"].String())
		if d := time.Since(began); took == 0 || d < took {
			took = d
		}
		if err != nil || len(week) != 50 || week[ids[49]].BlockedOpen != 1 || week[ids[49]].FirstBlocker == nil {
			t.Fatalf("a week of blocks: %d %v", len(week), err)
		}
	}
	t.Logf("CAL51-TB TIMING: 50 blocks' tasks with what they wait on, one read: best of 3 %v", took)
	if took > 2*time.Second {
		t.Errorf("a week of blocks read in %v", took)
	}
}
