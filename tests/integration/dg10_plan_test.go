//go:build integration

package integration_test

// DG10: a project's critical path and the impact of a move, as each reader
// may see them. Run: integ_rel.sh <wt> <log> 'TestDG10'

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

type dg10Plan struct {
	Data struct {
		Finish *string `json:"finish"`
		Tasks  []struct {
			UUID        string `json:"task_uuid"`
			External    bool   `json:"external"`
			Name        string `json:"task_name"`
			ProjectName string `json:"project_name"`
			Slack       int    `json:"slack"`
			Critical    bool   `json:"critical"`
		} `json:"tasks"`
		Links []struct {
			From, To string
			External bool `json:"external"`
			Critical bool `json:"critical"`
		} `json:"links"`
		Path      []string `json:"path"`
		CanEdit   bool     `json:"can_edit"`
		Truncated bool     `json:"truncated"`
	} `json:"data"`
}

type dg10Impact struct {
	Data struct {
		Delays struct{ Tasks, Projects int } `json:"delays"`
		Moves  []struct {
			UUID string `json:"task_uuid"`
		} `json:"moves"`
		Stuck  struct{ Tasks int }             `json:"stuck"`
		Finish struct{ Before, After *string } `json:"finish"`
	} `json:"data"`
}

func (w *dg10World) plan(who string) (dg10Plan, string) {
	w.t.Helper()
	code, body := w.call(who, false, taskController.CriticalPath, http.MethodGet, "/?tz=UTC", map[string]string{"project_uuid": w.p}, nil)
	if code != http.StatusOK {
		w.t.Fatalf("%s's critical path: %d %s", who, code, body)
	}
	var p dg10Plan
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		w.t.Fatal(err)
	}
	return p, body
}

func (w *dg10World) impact(who string, token bool, name string, start, due int) (int, dg10Impact, string) {
	w.t.Helper()
	q := url.Values{"start": {dg10Day(start, 9).Format(time.RFC3339)}, "due": {dg10Day(due, 17).Format(time.RFC3339)}, "tz": {"UTC"}}
	code, body := w.call(who, token, taskController.TaskImpact, http.MethodGet, "/?"+q.Encode(), map[string]string{"task_uuid": w.tasks[name]}, nil)
	var im dg10Impact
	_ = json.Unmarshal([]byte(body), &im)
	return code, im, body
}

// Launch's a → b → c, and Marketing's x waits on c: for Ana, who runs both,
// x ends the plan and the chain through it is critical; for Ben, who can't
// see Marketing, the plan ends at c, and nothing of x shows, not even a count.
func TestDG10CriticalPathAsEachReaderSeesIt(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("a", "p", 2, 4, "ana")
	w.task("b", "p", 5, 7, "", "a")
	w.task("c", "p", 8, 9, "", "b")
	w.task("d", "p", 1, 2, "")
	w.task("x", "q", 10, 12, "", "c")

	ana, _ := w.plan("ana")
	if ana.Data.Finish == nil || *ana.Data.Finish != "2026-11-12" {
		t.Fatalf("Ana's plan ends with Marketing's x: %+v", ana.Data.Finish)
	}
	want := []string{w.tasks["a"], w.tasks["b"], w.tasks["c"], w.tasks["x"]}
	if !reflect.DeepEqual(ana.Data.Path, want) {
		t.Errorf("Ana's critical path runs a, b, c, x: %v, want %v", ana.Data.Path, want)
	}
	for _, task := range ana.Data.Tasks {
		switch task.UUID {
		case w.tasks["x"]:
			if !task.External || task.Name != "x" || task.ProjectName != "Marketing" || !task.Critical {
				t.Errorf("x is Marketing's, named, and critical: %+v", task)
			}
		case w.tasks["d"]:
			if task.Slack != 10 || task.Critical {
				t.Errorf("d can slip 10 days: %+v", task)
			}
		}
	}
	crossed := false
	for _, l := range ana.Data.Links {
		if l.From == w.tasks["c"] && l.To == w.tasks["x"] {
			crossed = l.External && l.Critical
		}
	}
	if !crossed || !ana.Data.CanEdit {
		t.Errorf("the link into Marketing is drawn, critical; Ana may change Launch: %+v", ana.Data)
	}

	ben, body := w.plan("ben")
	if ben.Data.Finish == nil || *ben.Data.Finish != "2026-11-09" || strings.Contains(body, w.tasks["x"]) || strings.Contains(body, "Marketing") {
		t.Errorf("Ben's plan ends at c and says nothing of Marketing: %s", body)
	}
	if !reflect.DeepEqual(ben.Data.Path, want[:3]) || len(ben.Data.Links) != 2 {
		t.Errorf("Ben's path is a, b, c, with Launch's two links: %v %d", ben.Data.Path, len(ben.Data.Links))
	}
	if ben.Data.CanEdit != true {
		t.Errorf("Ben, a member, may change Launch's tasks")
	}
	if code, body := w.call("cat", false, taskController.CriticalPath, http.MethodGet, "/", map[string]string{"project_uuid": w.p}, nil); code != http.StatusOK || strings.Contains(body, `"can_edit":true`) {
		t.Errorf("Cat reads Launch, and may not change it: %d %s", code, body)
	}
}

// The impact line: what moving a would push along, by reader, and nothing
// written.
func TestDG10ImpactOfAMove(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	w.task("a", "p", 2, 4, "ana")
	w.task("b", "p", 5, 7, "", "a")
	w.task("c", "p", 8, 9, "", "b")
	w.task("x", "q", 10, 12, "", "c")
	// Marketing is admins-only: Ana runs it, so in the app she moves its x.
	w.exec(`INSERT INTO policy_scoped_values (scope_kind, scope_id, key, value) VALUES ('project', $1, 'projects.edit.who', 'admins')`, w.q)

	code, im, body := w.impact("ana", false, "a", 3, 5)
	if code != http.StatusOK || im.Data.Delays.Tasks != 3 || im.Data.Delays.Projects != 2 || im.Data.Stuck.Tasks != 0 {
		t.Fatalf("Ana's move delays b, c and x, in two projects: %d %s", code, body)
	}
	if im.Data.Finish.Before == nil || *im.Data.Finish.Before != "2026-11-09" || im.Data.Finish.After == nil || *im.Data.Finish.After != "2026-11-10" {
		t.Errorf("Launch would finish a day later: %s", body)
	}
	if s, _, _ := w.days("a"); s != 2 {
		t.Fatalf("an impact writes nothing: a starts on %d", s)
	}

	code, im, body = w.impact("ben", false, "a", 3, 5)
	if code != http.StatusOK || im.Data.Delays.Tasks != 2 || im.Data.Delays.Projects != 1 || im.Data.Stuck.Tasks != 0 || strings.Contains(body, w.tasks["x"]) {
		t.Errorf("Ben's move delays b and c; Marketing's x isn't counted anywhere: %d %s", code, body)
	}

	// Through a token, Ana is a member; in an admins-only Marketing she sees
	// x and can't change it: it's stuck, not moved.
	code, im, body = w.impact("ana", true, "a", 3, 5)
	if code != http.StatusOK || im.Data.Delays.Tasks != 2 || im.Data.Stuck.Tasks != 1 {
		t.Errorf("Ana's token: b and c move, x is stuck: %d %s", code, body)
	}

	if code, _, body := w.impact("cat", false, "a", 3, 5); code != http.StatusForbidden {
		t.Errorf("Cat only reads Launch: %d %s", code, body)
	}
}
