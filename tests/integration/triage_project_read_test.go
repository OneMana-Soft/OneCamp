//go:build integration
// +build integration

package integration_test

// A project's read carries its triage (linear-21 H1b), against real Postgres
// 12 and Dgraph, through the handler behind GET /project/info/{project_uuid}
// (what the app's project page reads): off, the read carries none; on, the
// project's people read project_triage {on, waiting}, waiting being what the
// queue (GET /project/{project_uuid}/triage) lists for them less what's
// snoozed; one outside the project is refused the read and sees none. A
// waiting task restricted from a reader is out of their count as it is out of
// their queue, and another project's waiting work is never counted.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestAProjectReadCarriesItsTriage|TestAProjectReadCountsOnlyTheTriageItsReaderMaySee)$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	triageController "github.com/akashc777/OneCamp/controllers/Triage"
)

// triageReadOf is who's read of w's project: its status, body and data.
func triageReadOf(t *testing.T, w *workWorld, who string) (int, string, map[string]any) {
	t.Helper()
	code, body := w.jfCall(who, projectController.ProjectInfo, http.MethodGet, "/", map[string]string{"project_uuid": w.project}, nil)
	var out struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return code, body, out.Data
}

// triageOf is who's project_triage on w's project, failing the test when
// the read fails.
func triageOf(t *testing.T, w *workWorld, who string) (any, bool) {
	t.Helper()
	code, body, d := triageReadOf(t, w, who)
	if code != http.StatusOK || d == nil {
		t.Fatalf("%s reads the project: %d %s", who, code, body)
	}
	got, ok := d["project_triage"]
	return got, ok
}

// triageWant fails unless who reads project_triage {on: true, waiting: n}.
func triageWant(t *testing.T, w *workWorld, who string, n int) {
	t.Helper()
	got, ok := triageOf(t, w, who)
	m, _ := got.(map[string]any)
	if !ok || m["on"] != true || m["waiting"] != float64(n) || len(m) != 2 {
		t.Fatalf("%s reads project_triage %#v, want {on: true, waiting: %d}", who, got, n)
	}
}

// triageQueueOf is how many of what w's project's queue lists for who
// aren't snoozed, and how many it lists.
func triageQueueOf(t *testing.T, w *workWorld, who string) (awake, listed int) {
	t.Helper()
	code, body := w.jfCall(who, triageController.Queue, http.MethodGet, "/", map[string]string{"project_uuid": w.project}, nil)
	var out struct {
		Data struct {
			Requests []map[string]any `json:"requests"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); code != http.StatusOK || err != nil {
		t.Fatalf("%s reads the queue: %d %s", who, code, body)
	}
	for _, r := range out.Data.Requests {
		if until, _ := r["snoozed_until"].(string); until == "" {
			awake++
		}
	}
	return awake, len(out.Data.Requests)
}

// triageTurn turns project's triage on or off, as ana.
func triageTurn(t *testing.T, w *workWorld, project string, on bool) {
	t.Helper()
	ana := w.account("ana")
	v, _ := json.Marshal(on)
	if _, err := policyBusiness.Admin.SetIn(context.Background(), policyBusiness.ProjectsTriage.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: project}, v,
		policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: "ana@acme.test"}); err != nil {
		t.Fatal(err)
	}
}

// triageHoldAll puts tasks in project's triage, as work from the API.
func triageHoldAll(t *testing.T, project string, tasks ...string) {
	t.Helper()
	for _, task := range tasks {
		if held, err := triageBusiness.Hold(context.Background(), uuid.MustParse(project), uuid.MustParse(task), triageBusiness.FromAPI, triageBusiness.Requester{}); err != nil || !held {
			t.Fatalf("holding %s: %v %v", task, held, err)
		}
	}
}

func TestAProjectReadCarriesItsTriage(t *testing.T) {
	w := seedWorkWorld(t)

	// Off, as every project starts: the read carries none (the app reads that as off).
	if got, ok := triageOf(t, w, "ana"); ok {
		t.Fatalf("triage off, ana reads project_triage %#v", got)
	}

	// On with nothing waiting.
	triageTurn(t, w, w.project, true)
	triageWant(t, w, "ana", 0)

	// Three come in from outside; ana snoozes one till tomorrow: the queue
	// lists three, and two wait.
	triageHoldAll(t, w.project, w.task, w.assigned, w.mine)
	until := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if code, body := w.jfCall("ana", triageController.Decide, http.MethodPost, "/", map[string]string{"task_uuid": w.mine},
		map[string]string{"decision": "snooze", "until": until}); code != http.StatusOK {
		t.Fatalf("ana snoozes one: %d %s", code, body)
	}
	for _, who := range []string{"ana", "ben"} {
		if n, listed := triageQueueOf(t, w, who); n != 2 || listed != 3 {
			t.Fatalf("%s's queue lists %d, %d of them not snoozed; want 3 and 2", who, listed, n)
		}
		triageWant(t, w, who, 2)
	}

	// Ben as a viewer still reads the queue, and so its count.
	if code, body := w.setRole("ana", "ben", "viewer"); code != http.StatusOK {
		t.Fatalf("ben as a viewer: %d %s", code, body)
	}
	triageWant(t, w, "ben", 2)

	// One the queue decides out of the count: two become one.
	if code, body := w.jfCall("ana", triageController.Decide, http.MethodPost, "/", map[string]string{"task_uuid": w.task},
		map[string]string{"decision": "accept"}); code != http.StatusOK {
		t.Fatalf("ana accepts one: %d %s", code, body)
	}
	if n, _ := triageQueueOf(t, w, "ben"); n != 1 {
		t.Fatalf("after the accept, ben's queue has %d not snoozed, want 1", n)
	}
	triageWant(t, w, "ben", 1)

	// Cy, in the team but not the project, is refused the read, and sees no triage.
	if code, body, _ := triageReadOf(t, w, "cy"); code != http.StatusForbidden || strings.Contains(body, "project_triage") {
		t.Fatalf("cy reads the project: %d %s", code, body)
	}

	// Off again: none, though one still waits.
	triageTurn(t, w, w.project, false)
	if got, ok := triageOf(t, w, "ana"); ok {
		t.Fatalf("triage turned off, ana reads project_triage %#v", got)
	}
}

// The count is the reader's own queue's: a waiting task restricted from a
// member is out of both (CR-355's probe), its project's admin still counts
// it, a grant brings it back, and work waiting in another project never
// counts.
func TestAProjectReadCountsOnlyTheTriageItsReaderMaySee(t *testing.T) {
	w := seedWorkWorld(t)
	triageTurn(t, w, w.project, true)
	triageHoldAll(t, w.project, w.task, w.assigned)

	// Another project of the team, triage on, with work of its own waiting.
	other := uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Support', $2, $3)`, other, w.team, w.people["ana"])
	triageTurn(t, w, other, true)
	triageHoldAll(t, other, uuid.NewString(), uuid.NewString(), uuid.NewString())

	same := func(who string, want int) {
		t.Helper()
		if n, listed := triageQueueOf(t, w, who); n != want || listed != want {
			t.Fatalf("%s's queue lists %d (%d not snoozed), want %d", who, listed, n, want)
		}
		triageWant(t, w, who, want)
	}
	same("ana", 2)
	same("ben", 2)

	// Ana restricts her waiting task to nobody else: ben's queue loses it, and
	// so does his count; ana runs the project and still sees and counts it.
	restrict := func(people []string) {
		t.Helper()
		if code, body := w.jfCall("ana", taskRestrictionController.SetRestriction, http.MethodPut, "/", map[string]string{"task_ref": w.task},
			map[string]any{"restricted": true, "people": people}); code != http.StatusOK {
			t.Fatalf("ana restricts her waiting task to %v: %d %s", people, code, body)
		}
	}
	restrict([]string{})
	same("ben", 1)
	same("ana", 2)

	// Let in by name, ben reads it again, in both.
	restrict([]string{w.people["ben"].String()})
	same("ben", 2)
}
