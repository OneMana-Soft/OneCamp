//go:build integration

package integration_test

// UX-PROJECTS-H1-BE (PROJECTS-14): "No priority" is a real choice, through the
// app's own handlers against real Postgres and Dgraph:
//   - a task made without a priority has none, whether made alone, as a
//     sub-task or many at once; before, each was made medium;
//   - No priority from a row's menu takes a task's priority off: the graph
//     keeps no value (has() sees none) and search is told to forget it.
//     Before, the handler refused "" as a missing field, so the row menu's
//     No priority failed with a 400;
//   - No priority for a selection takes it off every task, in the graph and
//     in search; before, "" was written over each priority and kept nothing;
//   - a task made through the API or an agent's tool without one has none,
//     and its answer says no "(priority: )";
//   - the next copy of a repeating task with no priority has none.
//
// Run: integ_rel.sh <wt> <log> '^TestNoPriorityIsARealChoice$'

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNoPriorityIsARealChoice(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	// Search keeps what it is sent.
	var mu sync.Mutex
	var sent []string
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, string(b))
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	integration.UseOpenSearchClient(t, client)

	ana := w.account("ana")
	post := func(what string, h http.HandlerFunc, body map[string]any, out any) {
		t.Helper()
		if code := pastCall(t, h, http.MethodPost, "/", body, ana, nil, out); code != http.StatusOK {
			t.Fatalf("%s: %d", what, code)
		}
	}
	priority := func(task string) (string, bool) { t.Helper(); return w.priorityOf(t, task) }
	none := func(what, task string) {
		t.Helper()
		if p, held := priority(task); p != "" || held {
			t.Errorf("%s: priority %q (held %v), want none", what, p, held)
		}
	}
	// forgotten is whether search was told the task has no priority.
	forgotten := func(task string) bool {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			mu.Lock()
			for _, b := range sent {
				for _, line := range strings.Split(b, "\n") {
					if strings.Contains(line, task) && strings.Contains(line, `"task_priority":null`) {
						mu.Unlock()
						return true
					}
				}
			}
			mu.Unlock()
		}
		return false
	}

	// Made alone, with no priority: none.
	var made struct {
		Task string `json:"task_uuid"`
	}
	post("create", taskController.CreateTask, map[string]any{"task_project_uuid": w.project, "task_name": "Plan the launch"}, &made)
	none("a task made without a priority", made.Task)

	// Made through the API (POST /v1/tasks) or by an agent's tool with no
	// priority: none, and the answer says no "(priority: )". Before, the API
	// made it medium. Given one, the answer says it.
	aiBusiness.RegisterToolExecutors()
	createTool, ok := ai.GetExecutor("create_task")
	if !ok {
		t.Fatal("create_task isn't registered")
	}
	madeBy := map[string]func(params map[string]string) v1Answer{
		"the API": func(params map[string]string) v1Answer {
			return callV1(t, context.Background(), ana, v1Controller.CreateTaskV1, "", params)
		},
		"an agent's tool": func(params map[string]string) v1Answer {
			msg, data, err := createTool(context.Background(), ai.ProposedAction{ToolName: "create_task", Params: params}, ana.UserDgraphInfo.Uuid)
			if err != nil {
				t.Fatalf("an agent's create: %v", err)
			}
			return v1Answer{Message: msg, Data: data}
		},
	}
	for by, create := range madeBy {
		plain := create(map[string]string{"task_name": "Order the banners", "project_uuid": w.project})
		none("a task made by "+by+" without a priority", plain.Data["task_uuid"])
		if strings.Contains(plain.Message, "priority") {
			t.Errorf("%s's answer for a task with no priority: %q", by, plain.Message)
		}
		given := create(map[string]string{"task_name": "Book the band", "project_uuid": w.project, "priority": "urgent"})
		if p, _ := priority(given.Data["task_uuid"]); p != "urgent" || !strings.Contains(given.Message, "(priority: urgent)") {
			t.Errorf("a task made by %s as urgent: %q, answer %q", by, p, given.Message)
		}
	}

	// Repeated: done, a task with no priority comes back with none, as it was
	// (CR-B61 M18). The rule moves to the next copy.
	post("repeat", taskController.SetTaskRecurrence, map[string]any{"task_uuid": made.Task, "rule": "FREQ=DAILY", "tz": "UTC"}, nil)
	post("done", taskController.UpdateTaskStatus, map[string]any{"task_uuid": made.Task, "task_project_uuid": w.project, "task_status": "done"}, nil)
	var next string
	if !within(10*time.Second, func() bool {
		return w.env.PG.QueryRow(`SELECT task_uuid FROM task_recurrences WHERE created_by = $1 AND task_uuid <> $2`, ana.UserPostgresInfo.Id, made.Task).Scan(&next) == nil
	}) {
		t.Fatal("done, the repeating task made no next copy")
	}
	none("the next copy of a repeating task with no priority", next)

	// Made as a sub-task: none.
	var sub struct {
		Task string `json:"task_uuid"`
	}
	post("sub-task", taskController.CreateSubTask, map[string]any{"task_project_uuid": w.project, "task_uuid": w.task, "task_name": "Book the venue"}, &sub)
	if sub.Task == "" {
		t.Fatal("the sub-task's answer names no task")
	}
	none("a sub-task made without a priority", sub.Task)

	// Made many at once (a board's notes, an assistant's proposals): none.
	var many struct {
		Made []string `json:"made"`
	}
	post("many", taskBulkController.CreateTasks, map[string]any{"project_uuid": w.project, "tasks": []map[string]any{{"task_name": "Draft the post"}, {"task_name": "Pick a date", "task_priority": ""}}}, &many)
	if len(many.Made) != 2 {
		t.Fatalf("made %v, want two", many.Made)
	}
	for _, id := range many.Made {
		none("one of many made without a priority", id)
	}

	// A priority set, then taken off from the row's menu.
	post("high", taskController.UpdateTaskPriority, map[string]any{"task_uuid": w.task, "task_priority": "high", "task_project_uuid": w.project}, nil)
	if p, _ := priority(w.task); p != "high" {
		t.Fatalf("set high: %q", p)
	}
	post("no priority", taskController.UpdateTaskPriority, map[string]any{"task_uuid": w.task, "task_priority": "", "task_project_uuid": w.project}, nil)
	none("No priority from the row's menu", w.task)
	if !forgotten(w.task) {
		t.Errorf("search wasn't told %s has no priority", w.task)
	}

	// Taken off a selection at once.
	pick := []string{w.mine, made.Task}
	post("bulk low", taskBulkController.Bulk, map[string]any{"task_uuids": pick, "op": "priority", "value": "low"}, nil)
	for _, id := range pick {
		if p, _ := priority(id); p != "low" {
			t.Fatalf("bulk low: %s is %q", id, p)
		}
	}
	var res struct {
		Changed []string `json:"changed"`
	}
	post("bulk no priority", taskBulkController.Bulk, map[string]any{"task_uuids": pick, "op": "priority", "value": ""}, &res)
	if len(res.Changed) != 2 {
		t.Errorf("bulk No priority changed %v, want both", res.Changed)
	}
	for _, id := range pick {
		none("No priority for a selection", id)
		if !forgotten(id) {
			t.Errorf("search wasn't told %s has no priority", id)
		}
	}
}

// priorityOf is a task's stored priority, and whether the graph holds one at
// all. The read names task_uuid too: the graph leaves out a node that has none
// of the predicates a block asks for, so a task with no priority would read
// as no task.
func (w *workWorld) priorityOf(t *testing.T, task string) (string, bool) {
	t.Helper()
	var got struct {
		Q []struct {
			Priority string `json:"task_priority"`
		} `json:"q"`
		Has []struct {
			UID string `json:"uid"`
		} `json:"has"`
	}
	q := fmt.Sprintf(`{ q(func: eq(task_uuid, %q)) { task_uuid task_priority } has(func: eq(task_uuid, %q)) @filter(has(task_priority)) { uid } }`, task, task)
	if err := json.Unmarshal(w.dg.Query(t, q), &got); err != nil || len(got.Q) != 1 {
		t.Fatalf("task %s in the graph: %v %+v", task, err, got)
	}
	return got.Q[0].Priority, len(got.Has) > 0
}
