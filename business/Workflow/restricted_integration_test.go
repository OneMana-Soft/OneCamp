//go:build integration

package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A rule on time never names or counts a restricted task (WA2). What a rule's
// runs say, every member of the project reads in its history, and a restricted
// task is shown only to its project's admins, its assignee, its creator and
// those granted on it; a rule is never told of one's events either. So a
// due-soon rule fires only for the task everyone may see, and a schedule that
// acts for each task matches and acts on that one alone, though its owner, an
// admin of the project, may see both.
//
// Run: PKGS=./business/Workflow/ integ_rel.sh <wt> <log> '^(TestARuleOnTimeNeverNamesARestrictedTask)$'
func TestARuleOnTimeNeverNamesARestrictedTask(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	owner, project, open, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'maya', 'Maya', NOW(), NOW())`,
		owner, owner.String()[:8]+"@example.test")
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, owner)
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3), ($4, $2, $3)`, open, project, owner, secret)
	// Acquiring Northwind is kept to a few people.
	exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, secret, project, owner)

	live := "0001-01-01T00:00:00Z"
	// Days from today: the sweep reads the test's clock, and the stores keep the real one.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	due := today.AddDate(0, 0, 1).Format(time.RFC3339)
	task := func(key string, id uuid.UUID, name string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": name,
			"task_status": "todo", "task_priority": "low", "task_deleted_at": live, "task_due_date": due,
			"task_project": map[string]any{"uid": "_:p"}, "task_created_by": map[string]any{"uid": "_:u"},
			"task_assignee": map[string]any{"uid": "_:u"}}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "maya", "user_deleted_at": live},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": "_:u"}}, "project_admins": []map[string]any{{"uid": "_:u"}},
			"project_tasks": []map[string]any{task("t1", open, "Ship the FAQ"), task("t2", secret, "Acquire Northwind")}},
	})

	oldClock := clock
	t.Cleanup(func() { clock = oldClock })
	clock = func() time.Time { return today.Add(10 * time.Hour) }
	notify := `[{"type":"notify","notify":"people","user_ids":["` + owner.String() + `"],"text":"{task} needs a look"}]`
	nudge, err := workflowModel.CreateWorkflow(ctx, owner, workflowModel.Fields{Name: "Nudge", IsActive: true, MatchType: "any",
		TriggerType: workflowModel.TriggerTaskDueSoon, TriggerConfigJSON: `{"days":1,"at":"09:00","tz":"UTC"}`, ProjectId: &project, ActionsJSON: notify})
	if err != nil {
		t.Fatal(err)
	}
	review, err := workflowModel.CreateWorkflow(ctx, owner, workflowModel.Fields{Name: "Review", IsActive: true, MatchType: "any",
		TriggerType: workflowModel.TriggerSchedule, TriggerConfigJSON: `{"repeat":"FREQ=DAILY","at":"10:00","from":"` + today.AddDate(0, 0, -9).Format(time.DateOnly) + `","tz":"UTC","each_task":true}`,
		ProjectId: &project, ActionsJSON: notify})
	if err != nil {
		t.Fatal(err)
	}
	if err := reload(ctx); err != nil {
		t.Fatal(err)
	}
	runs := func(id uuid.UUID) []*workflowModel.Run {
		t.Helper()
		rs, err := workflowModel.ListRuns(ctx, &project, &id, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	says := func(rs []*workflowModel.Run) string {
		b, _ := json.Marshal(rs)
		return string(b)
	}

	if err := sweepDue(ctx); err != nil {
		t.Fatal(err)
	}
	nudged := runs(nudge)
	if len(nudged) != 1 || nudged[0].SubjectID != open.String() {
		t.Errorf("the due-soon rule ran %d times: %s", len(nudged), says(nudged))
	}
	if strings.Contains(says(nudged), secret.String()) || strings.Contains(says(nudged), "Northwind") {
		t.Errorf("the due-soon rule's history names the restricted task: %s", says(nudged))
	}

	payload, _ := json.Marshal(workflowModel.SchedulePayload{WorkflowID: review.String()})
	if err := runScheduleJob(ctx, &jobModel.ScheduledJob{Id: uuid.New(), Payload: string(payload), RunAt: clock()}); err != nil {
		t.Fatal(err)
	}
	reviewed := runs(review)
	if len(reviewed) != 1 || reviewed[0].Matched == nil || *reviewed[0].Matched != 1 {
		t.Fatalf("the review's run counts what it matched as %s", says(reviewed))
	}
	for _, s := range reviewed[0].Steps {
		if s.TaskID != open.String() {
			t.Errorf("the review acted on %q: %s", s.TaskID, says(reviewed))
		}
	}
	if strings.Contains(says(reviewed), secret.String()) || strings.Contains(says(reviewed), "Northwind") {
		t.Errorf("the review's history names the restricted task: %s", says(reviewed))
	}
}
