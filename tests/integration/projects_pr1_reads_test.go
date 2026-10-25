//go:build integration

package integration_test

// PR1 (the BE audit's read paths): My Tasks across many projects asks
// Postgres a fixed number of times, however many projects its tasks are in
// (each project's own "Who can change tasks" was one statement per project,
// #2), and a list filtered by cycles reads their tasks in one statement (#15).
// Run: integ_rel.sh <wt> <log> 'TestPR1'

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// addProjects puts n more projects in the scale world's team, each with ben
// and ana in it and two tasks assigned to ben, one with its own "Who can
// change tasks".
func addProjects(t *testing.T, s *wa2Scale, n int) {
	t.Helper()
	live := "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	var team string
	if err := s.env.PG.QueryRow(`SELECT team_id FROM projects WHERE id = $1`, s.project).Scan(&team); err != nil {
		t.Fatal(err)
	}
	var nodes, onBen, onAna []map[string]any
	var pids []string
	taskProject := map[string]string{}
	for i := 0; i < n; i++ {
		pid := uuid.NewString()
		pids = append(pids, pid)
		pk := fmt.Sprintf("_:p%d", i)
		var tasks []map[string]any
		for j := 0; j < 2; j++ {
			tid := uuid.NewString()
			taskProject[tid] = pid
			tk := fmt.Sprintf("_:t%d_%d", i, j)
			nodes = append(nodes, map[string]any{"uid": tk, "dgraph.type": "Task", "task_uuid": tid, "task_name": fmt.Sprintf("Task %d.%d", i, j),
				"task_status": "todo", "task_priority": "medium", "task_deleted_at": live, "task_created_at": now.Format(time.RFC3339),
				"task_project": map[string]any{"uid": pk}, "task_created_by": map[string]any{"uid": s.uids["ana"]}, "task_assignee": map[string]any{"uid": s.uids["ben"]}})
			tasks = append(tasks, map[string]any{"uid": tk})
			onBen = append(onBen, map[string]any{"uid": tk})
		}
		nodes = append(nodes, map[string]any{"uid": pk, "dgraph.type": "Project", "project_uuid": pid, "project_name": fmt.Sprintf("Project %d", i), "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": s.uids["ana"]}, {"uid": s.uids["ben"]}}, "project_admins": []map[string]any{{"uid": s.uids["ana"]}},
			"project_created_by": map[string]any{"uid": s.uids["ana"]}, "project_tasks": tasks})
		onAna = append(onAna, map[string]any{"uid": pk})
	}
	nodes = append(nodes, map[string]any{"uid": s.uids["ben"], "user_tasks": onBen, "user_projects": onAna}, map[string]any{"uid": s.uids["ana"], "user_projects": onAna})
	s.dg.Mutate(t, nodes)
	for _, pid := range pids {
		s.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1::uuid, 'P ' || $1::text, $2, $3)`, pid, team, s.people["ana"])
	}
	for tid, pid := range taskProject {
		s.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, tid, pid, s.people["ana"])
	}
	// One in three projects has its own choice.
	for i, pid := range pids {
		if i%3 == 0 {
			s.exec(`INSERT INTO policy_scoped_values (scope_kind, scope_id, key, value) VALUES ('project', $1, 'projects.edit.who', 'assigned')`, pid)
		}
	}
}

func TestPR1MyTasksAcrossManyProjectsAsksPostgresAFixedNumberOfTimes(t *testing.T) {
	env := integration.SetupEnv(t)
	counter := integration.CountPostgres(t, env)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.counter = counter
	count := func() int {
		t.Helper()
		read := func() {
			if code, out := s.read("ben", userController.GetDgraphUserTaskList, "?pageSize=200&pageIndex=0"); code != http.StatusOK {
				t.Fatalf("my tasks: %d %s", code, out)
			}
		}
		read()
		counter.Reset()
		// The policies' cache keeps a project's values 15 s: a cold read is
		// what's counted, so the cache is emptied first by waiting it out.
		time.Sleep(16 * time.Second)
		counter.Reset()
		read()
		return counter.Count()
	}
	addProjects(t, s, 5)
	few := count()
	addProjects(t, s, 45)
	many := count()
	t.Logf("My Tasks sends Postgres %d statements across 5 projects, %d across 50", few, many)
	if many != few {
		t.Fatalf("%d statements across 50 projects, %d across 5: a read asks per project", many, few)
	}
}
