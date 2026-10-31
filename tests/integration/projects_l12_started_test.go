//go:build integration

package integration_test

// linear-12: in a project whose cycles run themselves, with add_started on, a
// task that starts joins the cycle running then (business/Cycle/startedJoins.go).
// The setting is the schedule's, so only the project's admins set it.
// Run: integ_rel.sh <wt> <log> 'TestLinear12AStartedTaskJoinsTheRunningCycle'

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestLinear12AStartedTaskJoinsTheRunningCycle(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(6) // statuses go round todo, inProgress, inReview, backlog, done, canceled

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	in := map[string]any{"weeks": 1, "upcoming": 2, "roll_over": true, "add_started": true, "starts_at": today.AddDate(0, 0, -3)}

	// The project's admins set it; a member can't.
	if code, out := s.postAs("ben", cycleController.SetCycleSchedule, in); code != http.StatusForbidden {
		t.Fatalf("a member set the schedule: %d %s", code, out)
	}
	if code, out := s.postAs("ana", cycleController.SetCycleSchedule, in); code != http.StatusOK || !strings.Contains(strings.Join(strings.Fields(out), ""), `"add_started":true`) {
		t.Fatalf("set the schedule: %d %s", code, out)
	}
	code, out := s.read("ben", cycleController.ListCycles, "")
	if code != http.StatusOK || !strings.Contains(strings.Join(strings.Fields(out), ""), `"add_started":true`) {
		t.Fatalf("the schedule as read: %d %s", code, out)
	}
	cycles, _ := s.cycles("ben")
	if len(cycles) == 0 || cycles[0].State != "current" {
		t.Fatalf("no running cycle: %+v", cycles)
	}

	// A to-do task started through its own route joins the running cycle.
	todo := s.tasks[0]
	if code, out := s.postAs("ana", taskController.UpdateTaskStatus, map[string]string{"task_uuid": todo, "task_status": "inProgress", "task_project_uuid": s.project}); code != http.StatusOK {
		t.Fatalf("starting the task: %d %s", code, out)
	}
	// The listener runs after the change is answered (business/Webhook): wait for it.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var cycle string
		err := s.env.PG.QueryRowContext(context.Background(), `SELECT cycle_id::text FROM task_cycles WHERE task_uuid = $1`, todo).Scan(&cycle)
		if err == nil {
			if cycle != cycles[0].ID {
				t.Fatalf("the started task joined %s, want the running cycle %s", cycle, cycles[0].ID)
			}
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the started task never joined the running cycle")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
