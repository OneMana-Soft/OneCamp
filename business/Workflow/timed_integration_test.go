//go:build integration

package business

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A due date fires a rule once, read from the graph by the sweep: a second
// pass finds the run made, and a date moved to another day fires again.
func TestDueSoonFiresOncePerDueDate(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	owner, project, task := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'maya', 'Maya', NOW(), NOW())`,
		owner, owner.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	live := "0001-01-01T00:00:00Z"
	dueOn := func(d time.Time) string { return d.UTC().Format(time.RFC3339) }
	// Days from today: the sweep reads the test's clock, and the stores keep the real one.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := today.AddDate(0, 0, 1)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "maya", "user_deleted_at": live},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:u"}}, "project_admins": []map[string]any{{"uid": "_:u"}},
			"project_tasks": []map[string]any{{"uid": "_:t", "dgraph.type": "Task", "task_uuid": task.String(), "task_name": "Ship the FAQ",
				"task_status": "todo", "task_priority": "low", "task_deleted_at": live, "task_due_date": dueOn(day),
				"task_project": map[string]any{"uid": "_:p"}, "task_created_by": map[string]any{"uid": "_:u"},
				"task_assignee": map[string]any{"uid": "_:u"}}}},
	})

	oldClock := clock
	t.Cleanup(func() { clock = oldClock })
	clock = func() time.Time { return today.Add(10 * time.Hour) }
	id, err := workflowModel.CreateWorkflow(ctx, owner, workflowModel.Fields{Name: "Nudge", IsActive: true, MatchType: "any",
		TriggerType: workflowModel.TriggerTaskDueSoon, TriggerConfigJSON: `{"days":1,"at":"09:00","tz":"UTC"}`, ProjectId: &project,
		ActionsJSON: `[{"type":"notify","notify":"people","user_ids":["` + owner.String() + `"],"text":"{task} is due tomorrow"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if err := reload(ctx); err != nil {
		t.Fatal(err)
	}
	runs := func() []*workflowModel.Run {
		t.Helper()
		rs, err := workflowModel.ListRuns(ctx, &project, &id, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	for i := 0; i < 2; i++ {
		if err := sweepDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rs := runs()
	if len(rs) != 1 || rs[0].Status != workflowModel.RunOK || rs[0].SubjectID != task.String() {
		t.Fatalf("after two passes: %+v", rs)
	}

	// Moved a day on: due soon again on its eve at 09:00, and not before.
	taskUID := uids["t"]
	if taskUID == "" {
		t.Fatalf("no uid for the task: %v", uids)
	}
	dg.Mutate(t, []map[string]any{{"uid": taskUID, "task_due_date": dueOn(day.AddDate(0, 0, 1))}})
	if err := sweepDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(runs()); n != 1 {
		t.Fatalf("fired a day early: %d runs", n)
	}
	clock = func() time.Time { return day.Add(9*time.Hour + 5*time.Minute) }
	if err := sweepDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(runs()); n != 2 {
		t.Errorf("the moved date fired %d times in all, want 2", n-1)
	}
}

// The hourly pass plans a live schedule rule whose job went missing.
func TestTheHourlyPassPlansAScheduleLeftWithoutAJob(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	owner, project := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, owner, owner.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	id, err := workflowModel.CreateWorkflow(ctx, owner, workflowModel.Fields{Name: "Monday", IsActive: true, MatchType: "any",
		TriggerType: workflowModel.TriggerSchedule, TriggerConfigJSON: `{"repeat":"FREQ=WEEKLY;BYDAY=MO","at":"09:00","from":"` + time.Now().UTC().AddDate(0, 0, -4).Format(time.DateOnly) + `"}`,
		ProjectId: &project, ActionsJSON: `[{"type":"create_task","project_id":"` + project.String() + `","task_name":"Review"}]`})
	if err != nil {
		t.Fatal(err)
	}
	waiting := func() int {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE job_type = 'workflow_schedule' AND status = 'pending'
			AND payload->>'workflow_id' = $1`, id.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := waiting(); n != 0 {
		t.Fatalf("%d jobs before the pass", n)
	}
	for i := 0; i < 2; i++ {
		if err := replanSchedules(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := waiting(); n != 1 {
		t.Fatalf("after two passes, %d jobs wait, want 1", n)
	}
	if err := workflowModel.SetWorkflowActive(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := syncSchedule(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := replanSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	if n := waiting(); n != 0 {
		t.Errorf("a rule turned off was planned: %d", n)
	}
}
