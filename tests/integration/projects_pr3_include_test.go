//go:build integration

package integration_test

// PR3 (the BE audit's panel fan-out, #10): the task panel's other reads come
// with the task when /task/info asks for them (?include=), each exactly as its
// own endpoint answers it, so the app makes one request where it made eight,
// two levels deep.
// Run: integ_rel.sh <wt> <log> 'TestPR3TaskInfoIncludesThePanelsReads'

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	estimateScaleController "github.com/akashc777/OneCamp/controllers/EstimateScale"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	taskStatusController "github.com/akashc777/OneCamp/controllers/TaskStatus"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	"github.com/akashc777/OneCamp/tests/integration"
)

// readWith runs a handler as who with the given route parameters.
func (s *wa2Scale) readWith(who string, h http.HandlerFunc, params map[string]string, query string) (int, string) {
	s.t.Helper()
	return s.call(who, http.MethodGet, "/"+query, h, params, nil)
}

func TestPR3TaskInfoIncludesThePanelsReads(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.counter = counter
	s.addTasks(6)
	task := s.tasks[0]

	// Something in every part: a cycle, a repeat, an hour of Ben's time.
	now := time.Now().UTC()
	var cycle string
	if err := env.PG.QueryRow(`INSERT INTO project_cycles (project_uuid, number, name, starts_at, ends_at, created_by)
		VALUES ($1, 1, 'Cycle 1', $2, $3, $4) RETURNING id`, s.project, now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"]).Scan(&cycle); err != nil {
		t.Fatal(err)
	}
	s.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, task, cycle)
	s.exec(`INSERT INTO task_recurrences (task_uuid, rule, created_by) VALUES ($1, 'FREQ=WEEKLY', $2)`, task, s.people["ana"])
	s.exec(`INSERT INTO task_time_entries (task_uuid, project_uuid, user_id, started_at, ended_at) VALUES ($1, $2, $3, $4, $5)`,
		task, s.project, s.people["ben"], now.Add(-2*time.Hour), now.Add(-time.Hour))

	onTask := map[string]string{"task_uuid": task}
	onProject := map[string]string{"project_uuid": s.project}
	alone := map[string]struct {
		h      http.HandlerFunc
		params map[string]string
	}{
		"cycle":         {cycleController.GetTaskCycle, onTask},
		"recurrence":    {taskController.GetTaskRecurrence, onTask},
		"client_review": {guestController.TaskClientReview, onTask},
		"time_summary":  {timeEntryController.GetTaskTime, onTask},
		"statuses":      {taskStatusController.List, onProject},
		"fields":        {taskFieldController.ListFields, onProject},
		"estimates":     {estimateScaleController.GetScale, onProject},
	}
	const include = "?include=cycle,recurrence,client_review,time_summary,sync_status,project_meta"

	// What each endpoint answers on its own, and what that costs.
	want := map[string]json.RawMessage{}
	separate := 0
	for name, read := range alone {
		s.readWith("ben", read.h, read.params, "") // warm what is kept a while
		counter.Reset()
		code, out := s.readWith("ben", read.h, read.params, "")
		separate += counter.Count()
		if code != http.StatusOK {
			t.Fatalf("%s alone: %d %s", name, code, out)
		}
		var body struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatal(err)
		}
		want[name] = body.Data
	}
	s.readWith("ben", taskController.GetTaskInfo, onTask, "")
	counter.Reset()
	if code, out := s.readWith("ben", taskController.GetTaskInfo, onTask, ""); code != http.StatusOK {
		t.Fatalf("task info: %d %s", code, out)
	}
	separate += counter.Count()

	s.readWith("ben", taskController.GetTaskInfo, onTask, include)
	counter.Reset()
	code, out := s.readWith("ben", taskController.GetTaskInfo, onTask, include)
	together := counter.Count()
	if code != http.StatusOK {
		t.Fatalf("task info with its parts: %d %s", code, out)
	}
	var got struct {
		Data struct {
			UUID string `json:"task_uuid"`
		} `json:"data"`
		Included map[string]struct {
			Data json.RawMessage `json:"data"`
		} `json:"included"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.UUID != task {
		t.Fatalf("the task itself: %q, want %q", got.Data.UUID, task)
	}
	for name, w := range want {
		part, ok := got.Included[name]
		if !ok {
			t.Errorf("%s: not included (%s)", name, out)
			continue
		}
		if !sameJSON(t, part.Data, w) {
			t.Errorf("%s: included %s, alone %s", name, part.Data, w)
		}
	}
	// A task with no GitHub link has no sync status to show.
	if _, ok := got.Included["sync_status"]; ok {
		t.Errorf("sync_status included for a task with no GitHub link")
	}
	t.Logf("Postgres statements: %d for the task and its seven reads alone, %d together", separate, together)
	if together >= separate {
		t.Errorf("together %d statements, alone %d: including the parts should spare their own access checks", together, separate)
	}

	// Without ?include= the answer is as it was: no parts.
	_, plain := s.readWith("ben", taskController.GetTaskInfo, onTask, "")
	var bare map[string]json.RawMessage
	_ = json.Unmarshal([]byte(plain), &bare)
	if _, ok := bare["included"]; ok {
		t.Errorf("a plain read carries included: %s", plain)
	}

	// A task kept from Ben answers as no task, parts and all.
	if s.restrict(1) {
		code, out := s.readWith("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": s.tasks[2]}, include)
		if code != http.StatusNotFound {
			t.Fatalf("a task kept from ben: %d %s", code, out)
		}
	}
}

// sameJSON compares two JSON values by meaning, not spelling.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}
