//go:build integration

package integration_test

// A cycle's counts are what its reader may see (V-20). The project's cycle
// list, the running cycles across projects and a cycle's burndown count the
// cycle's live tasks the reader may see, left out in the query: a member kept
// from a task gets counts without it, and whoever may see every task gets
// them all.
// Run: integ_rel.sh <wt> <log> 'TestV20CycleCountsAreWhatTheReaderMaySee'

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestV20CycleCountsAreWhatTheReaderMaySee(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(4)
	if !s.restrict(1) {
		t.Fatal("task_restrictions is missing: this test needs it")
	}
	// The cycle running now holds all four tasks; the third is kept from Ben.
	now := time.Now().UTC()
	var cycle string
	if err := env.PG.QueryRow(`INSERT INTO project_cycles (project_uuid, number, name, starts_at, ends_at, created_by)
		VALUES ($1, 1, 'Cycle 1', $2, $3, $4) RETURNING id`, s.project, now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"]).Scan(&cycle); err != nil {
		t.Fatal(err)
	}
	for _, task := range s.tasks {
		s.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, task, cycle)
	}
	if code, out := s.readWith("ben", taskController.GetTaskInfo, map[string]string{"task_uuid": s.tasks[2]}, ""); code != http.StatusNotFound {
		t.Fatalf("the third task is kept from Ben: %d %s", code, out)
	}

	type progress struct {
		Total int `json:"total"`
	}
	listed := func(who string) int {
		t.Helper()
		code, out := s.readWith(who, cycleController.ListCycles, map[string]string{"project_uuid": s.project}, "")
		var got struct {
			Data struct {
				Cycles []struct {
					ID       string   `json:"id"`
					Progress progress `json:"progress"`
				} `json:"cycles"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); code != http.StatusOK || err != nil || len(got.Data.Cycles) != 1 {
			t.Fatalf("%s lists the cycles: %d %v %s", who, code, err, out)
		}
		return got.Data.Cycles[0].Progress.Total
	}
	running := func(who string) int {
		t.Helper()
		code, out := s.readWith(who, cycleController.RunningCycles, nil, "")
		var got struct {
			Data struct {
				Cycles []struct {
					Project  string   `json:"project_uuid"`
					Progress progress `json:"progress"`
				} `json:"cycles"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); code != http.StatusOK || err != nil {
			t.Fatalf("%s reads the running cycles: %d %v %s", who, code, err, out)
		}
		for _, c := range got.Data.Cycles {
			if c.Project == s.project {
				return c.Progress.Total
			}
		}
		t.Fatalf("%s's running cycles leave out the project: %s", who, out)
		return 0
	}
	burndown := func(who string) (int, int) {
		t.Helper()
		code, out := s.readWith(who, cycleController.CycleBurndown, map[string]string{"project_uuid": s.project, "cycle_id": cycle}, "")
		var got struct {
			Data struct {
				Cycle struct {
					Progress progress `json:"progress"`
				} `json:"cycle"`
				Burndown struct {
					Tasks int `json:"tasks"`
				} `json:"burndown"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); code != http.StatusOK || err != nil {
			t.Fatalf("%s reads the burndown: %d %v %s", who, code, err, out)
		}
		return got.Data.Cycle.Progress.Total, got.Data.Burndown.Tasks
	}

	t.Run("a member kept from a task counts the cycle without it", func(t *testing.T) {
		if n := listed("ben"); n != 3 {
			t.Errorf("Ben's cycle list counts %d tasks, want 3", n)
		}
		if n := running("ben"); n != 3 {
			t.Errorf("Ben's running cycles count %d tasks, want 3", n)
		}
		if p, b := burndown("ben"); p != 3 || b != 3 {
			t.Errorf("Ben's burndown counts %d and %d tasks, want 3 and 3", p, b)
		}
	})

	t.Run("whoever may see every task counts them all", func(t *testing.T) {
		if n := listed("ana"); n != 4 {
			t.Errorf("Ana's cycle list counts %d tasks, want 4", n)
		}
		if n := running("ana"); n != 4 {
			t.Errorf("Ana's running cycles count %d tasks, want 4", n)
		}
		if p, b := burndown("ana"); p != 4 || b != 4 {
			t.Errorf("Ana's burndown counts %d and %d tasks, want 4 and 4", p, b)
		}
	})
}
