//go:build integration

package business

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A request's answer and its reminders reach only people who may read its
// task when they're told, against real Postgres and Dgraph: they were seen
// when it was asked, and someone taken off the task's project since hears
// nothing of it, its name included. Ana and Fay are in the project; Ben asked
// and Lou was asked, and both have been taken off it since.
//
// Run: go test -tags=integration ./business/Approvals/ -run TestARequestsNoticesReachOnlyWhoMayStillReadTheTask -v
func TestARequestsNoticesReachOnlyWhoMayStillReadTheTask(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"ana", "fay", "ben", "lou"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	team, project := uuid.NewString(), uuid.NewString()
	decided, kept, due := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Web', $2)`, team, people["ana"])
	exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Website', $2, $3)`, project, team, people["ana"])
	var tasks []map[string]any
	for name, id := range map[string]uuid.UUID{"decided": decided, "kept": kept, "due": due} {
		exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, project, people["ana"])
		tasks = append(tasks, map[string]any{"uid": "_:" + name, "dgraph.type": "Task", "task_uuid": id.String(), "task_name": "Ship the " + name + " page",
			"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ana"}})
	}
	nodes = append(nodes, map[string]any{"uid": "_:website", "dgraph.type": "Project", "project_uuid": project, "project_name": "Website",
		"project_deleted_at": live, "project_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:fay"}},
		"project_admins": []map[string]any{{"uid": "_:ana"}}, "project_created_by": map[string]any{"uid": "_:ana"}, "project_tasks": tasks})
	dg.Mutate(t, nodes)

	open := func(task uuid.UUID, by string, approvers []string, dueAt *time.Time) *model.Approval {
		t.Helper()
		ids := make([]string, len(approvers))
		for i, a := range approvers {
			ids[i] = people[a].String()
		}
		a, created, err := model.Open(ctx, &model.Approval{ResourceKind: model.KindTask, ResourceID: task.String(), TaskID: task,
			ProjectID: uuid.MustParse(project), RequestedBy: people[by], Approvers: ids, Needed: 1, DueAt: dueAt})
		if err != nil || !created {
			t.Fatalf("open a request on %s: %v %v", task, created, err)
		}
		return a
	}
	told := func(name, kind string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2`, people[name], kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["ana"])
	if err != nil || pg == nil {
		t.Fatalf("load ana: %v", err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["ana"].String())
	if err != nil || node == nil {
		t.Fatalf("load ana's node: %v", err)
	}
	ana := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
	asAna := context.WithValue(taskread.Install(context.Background()), helpers.UserInfoContextKey, ana)

	t.Run("its answer tells the person who asked only while they may read the task", func(t *testing.T) {
		gone, stays := open(decided, "ben", []string{"ana"}, nil), open(kept, "fay", []string{"ana"}, nil)
		for _, a := range []*model.Approval{gone, stays} {
			if v, err := Decide(asAna, &ana, a.TaskID, a.ID, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
				t.Fatalf("ana approves: %+v %v", v, err)
			}
		}
		if !told("fay", "work.approval.decided") {
			t.Error("fay, in the project, wasn't told her request was approved")
		}
		if told("ben", "work.approval.decided") {
			t.Error("ben, taken off the project, was told how his request on its task ended")
		}
	})

	t.Run("its reminders reach only approvers and the person who asked who may read the task", func(t *testing.T) {
		at := time.Now().UTC().Truncate(time.Second)
		dueAt := at.Add(12 * time.Hour)
		open(due, "ben", []string{"ana", "lou"}, &dueAt)
		if err := remind(context.Background(), at); err != nil {
			t.Fatalf("a day before: %v", err)
		}
		if err := remind(context.Background(), dueAt.Add(time.Minute)); err != nil {
			t.Fatalf("once it's past due: %v", err)
		}
		if !told("ana", "work.approval.due") {
			t.Error("ana, in the project, wasn't reminded")
		}
		for _, name := range []string{"lou", "ben"} {
			if told(name, "work.approval.due") {
				t.Errorf("%s, taken off the project, was reminded of a request on its task", name)
			}
		}
	})
}
