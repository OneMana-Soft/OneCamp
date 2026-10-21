//go:build integration

package integration_test

// PR1's gaps: a project's glance counted by the server over all its tasks,
// in the reader's day, and a person's list asked for what is due by a time
// (Home's "needs me").
// Run: integ_rel.sh <wt> <log> 'TestPR1B'

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestPR1BGlanceAndDueByCountEverything(t *testing.T) {
	env := integration.SetupEnv(t)
	integration.CountPostgres(t, env) // points the server's pool at this database
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(120)

	// What the seed made (wa2Scale.addTasks): status by k%6 (open below 4),
	// a due date now+(k%20-5) days on even k, ben assigned on k%3 == 0.
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	seeded := func(k int) (open bool, due time.Time, dated bool) {
		open = k%6 < 4
		if k%2 == 0 {
			due, dated = now.AddDate(0, 0, k%20-5), true
		}
		return
	}
	var wantOpen, wantDone, wantOverdue, wantWeek, wantBen int
	endOfToday := today.AddDate(0, 0, 1)
	for k := 0; k < 120; k++ {
		open, due, dated := seeded(k)
		if open {
			wantOpen++
		} else if k%6 == 4 {
			wantDone++
		}
		if open && dated && due.Before(today) {
			wantOverdue++
		}
		if open && dated && !due.Before(today) && due.Before(today.AddDate(0, 0, 7)) {
			wantWeek++
		}
		if open && dated && k%3 == 0 && !due.After(endOfToday) {
			wantBen++
		}
	}

	code, out := s.read("ben", projectController.ProjectGlance, "?tz=UTC")
	if code != http.StatusOK {
		t.Fatalf("glance: %d %s", code, out)
	}
	var g struct {
		Data struct {
			Open, Done, Overdue int
			Week                int `json:"due_this_week"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		t.Fatal(err)
	}
	if g.Data.Open != wantOpen || g.Data.Done != wantDone || g.Data.Overdue != wantOverdue || g.Data.Week != wantWeek {
		t.Fatalf("glance %+v, want open %d done %d overdue %d week %d", g.Data, wantOpen, wantDone, wantOverdue, wantWeek)
	}

	// The one filter builder: a search and a status, on the project's list.
	filters := url.QueryEscape(`[{"id":"task_status","value":["todo"]}]`)
	code, out = s.read("ben", projectController.GetProjectTaskList, "?pageSize=200&pageIndex=0&taskSearchString=Task%2011&filters="+filters)
	var list struct {
		Data struct {
			Count int `json:"project_task_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != http.StatusOK {
		t.Fatalf("list: %d %s", code, out)
	}
	// "Task 11" names 11 and 110 to 119; of those only 114 is in To do (k%6 == 0).
	if list.Data.Count != 1 {
		t.Fatalf("searched and filtered list: %d tasks, want 1", list.Data.Count)
	}

	code, out = s.read("ben", userController.GetDgraphUserTaskList, "?pageSize=200&pageIndex=0&dueBy="+url.QueryEscape(endOfToday.Format(time.RFC3339)))
	if code != http.StatusOK {
		t.Fatalf("due by: %d %s", code, out)
	}
	var mine struct {
		Data struct {
			Tasks []struct {
				UUID string `json:"task_uuid"`
			} `json:"user_tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Data.Tasks) != wantBen {
		t.Fatalf("ben's tasks due by the end of today: %d, want %d", len(mine.Data.Tasks), wantBen)
	}
}
