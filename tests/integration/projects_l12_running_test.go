//go:build integration

package integration_test

// linear-12 part 3: every project's running cycle in one list
// (business/Cycle/running.go). A person sees the cycles of the projects they
// may view, with each one's progress; a project they aren't in stays out.
// Run: integ_rel.sh <wt> <log> 'TestLinear12CRunningCyclesAcrossProjects'

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestLinear12CRunningCyclesAcrossProjects(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(6) // todo, inProgress, inReview, backlog, done, canceled

	// A second project only Ana is in, in the same team.
	other := uuid.NewString()
	var team string
	if err := env.PG.QueryRow(`SELECT team_id::text FROM projects WHERE id = $1`, s.project).Scan(&team); err != nil {
		t.Fatal(err)
	}
	s.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Ana alone', $2, $3)`, other, team, s.people["ana"])
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:q", "dgraph.type": "Project", "project_uuid": other, "project_name": "Ana alone", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": s.uids["ana"]}}, "project_admins": []map[string]any{{"uid": s.uids["ana"]}},
			"project_created_by": map[string]any{"uid": s.uids["ana"]}},
		{"uid": s.uids["ana"], "user_projects": []map[string]any{{"uid": "_:q"}}},
	})

	// A cycle running now in each, and one in the big board that has ended.
	now := time.Now().UTC()
	board, alone := uuid.New(), uuid.New()
	s.exec(`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at) VALUES
		($1, $3, 1, 'Ended', $5, $6), ($2, $3, 2, 'Board cycle', $6, $7), ($8, $4, 1, 'Alone cycle', $6, $7)`,
		uuid.New(), board, s.project, other, now.AddDate(0, 0, -14), now.AddDate(0, 0, -7), now.AddDate(0, 0, 7), alone)
	// Four of the board's tasks in its cycle: to do, in progress, in review, done.
	for _, i := range []int{0, 1, 2, 4} {
		s.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, s.tasks[i], board)
	}

	type row struct {
		ID          string `json:"id"`
		ProjectUUID string `json:"project_uuid"`
		ProjectName string `json:"project_name"`
		Name        string `json:"name"`
		State       string `json:"state"`
		Pace        string `json:"pace"`
		DaysLeft    int    `json:"days_left"`
		Progress    struct {
			Total, Started, Done int
		} `json:"progress"`
	}
	running := func(who string) map[string]row {
		t.Helper()
		code, out := s.read(who, cycleController.RunningCycles, "")
		if code != http.StatusOK {
			t.Fatalf("running cycles as %s: %d %s", who, code, out)
		}
		var got struct {
			Data struct {
				Cycles []row `json:"cycles"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		by := map[string]row{}
		for _, r := range got.Data.Cycles {
			by[r.ProjectUUID] = r
		}
		return by
	}

	// A busy workspace: 501 other projects' cycles running, every one ending
	// before the board's. Ben's and Ana's own still show (CR-68 MF1: a list read
	// workspace-wide, capped and then filtered, lost them).
	s.exec(`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at)
		SELECT uuid_generate_v4(), uuid_generate_v4(), 1, 'Busy', $1, $2 FROM generate_series(1, 501)`, now.AddDate(0, 0, -1), now.AddDate(0, 0, 1))
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`DELETE FROM project_cycles WHERE name = 'Busy' AND number = 1 AND starts_at = $1`, now.AddDate(0, 0, -1))
	})

	// Ana is in both projects: both running cycles, and not the ended one.
	ana := running("ana")
	if ana[s.project].ID != board.String() || ana[other].ID != alone.String() {
		t.Fatalf("Ana's running cycles: %+v", ana)
	}
	b := ana[s.project]
	if b.ProjectName != "Big board" || b.State != "current" || b.Progress.Total != 4 || b.Progress.Started != 2 || b.Progress.Done != 1 || b.DaysLeft != 7 {
		t.Fatalf("the board's cycle: %+v", b)
	}
	// Ben is in the board only: the other project's cycle isn't his to see.
	ben := running("ben")
	if ben[s.project].ID != board.String() {
		t.Fatalf("Ben's board cycle: %+v", ben)
	}
	if _, ok := ben[other]; ok {
		t.Fatalf("Ben sees the cycle of a project he isn't in: %+v", ben[other])
	}
}
