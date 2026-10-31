//go:build integration
// +build integration

package integration_test

// The projects programme's day-0 contracts answer as the handlers do, against
// real Postgres 12 and Dgraph (design/projects-vs-asana-jira/PROGRAM.md §5,
// and §9's "contracts guarded"): the Access Resolver's work kinds decide as
// Work Access does (since A1, members change tasks unless their project says
// otherwise; projects_a1_test.go has the roles and the policy), the Task
// Writer makes a change only for someone who may make it, and a policy's
// value for one project is kept apart from the rest.
//
// Run: go test -tags=integration ./tests/integration/ -run TestProjectsDay0ContractsAnswerAsToday -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestProjectsDay0ContractsAnswerAsToday(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()

	// The project: its admins run it (fay too, on its admins only), its member
	// does its work, nobody else is in.
	project := accessBusiness.Resource{Kind: workAccess.KindProject, UUID: w.project}
	for who, want := range map[string]accessBusiness.Level{
		"ana": accessBusiness.LevelFull, "ben": accessBusiness.LevelEdit, "cy": accessBusiness.LevelNone,
		"dee": accessBusiness.LevelNone, "eve": accessBusiness.LevelNone, "fay": accessBusiness.LevelFull,
	} {
		d, err := accessBusiness.Rules.Resolve(ctx, w.subject(who), project)
		if err != nil || d.Level != want {
			t.Errorf("%s on the project: %+v %v, want %s", who, d, err, want)
		}
	}

	// A task: the same, and its assignee outside the project reads it; a
	// deleted one answers none, with what it was.
	task := accessBusiness.Resource{Kind: workAccess.KindTask, UUID: w.assigned}
	for who, want := range map[string]accessBusiness.Level{"ana": accessBusiness.LevelFull, "ben": accessBusiness.LevelEdit, "eve": accessBusiness.LevelView, "cy": accessBusiness.LevelNone} {
		if d, err := accessBusiness.Rules.Resolve(ctx, w.subject(who), task); err != nil || d.Level != want {
			t.Errorf("%s on the task: %+v %v, want %s", who, d, err, want)
		}
	}
	if d, err := accessBusiness.Rules.Resolve(ctx, w.subject("ben"), accessBusiness.Resource{Kind: workAccess.KindTask, UUID: w.deleted}); err != nil || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelEdit {
		t.Errorf("a deleted task: %+v %v", d, err)
	}

	// A saved view is its owner's; a cycle and a form their project's.
	view, cycle, form := uuid.New(), uuid.New(), uuid.New()
	w.exec(`INSERT INTO task_views (id, user_id, scope, name, state) VALUES ($1, $2, 'project:' || $3, 'Mine', '{}')`, view, w.people["ben"], w.project)
	w.exec(`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at) VALUES ($1, $2, 1, 'Launch week', NOW(), NOW() + interval '7 days')`, cycle, w.project)
	w.exec(`INSERT INTO project_forms (id, project_uuid, token, title, fields, created_by) VALUES ($1, $2, 'tok', 'Requests', '[]', $3)`, form, w.project, w.people["ana"])
	for _, c := range []struct {
		who   string
		kind  string
		id    uuid.UUID
		level accessBusiness.Level
	}{
		{"ben", workAccess.KindView, view, accessBusiness.LevelFull},
		{"ana", workAccess.KindView, view, accessBusiness.LevelNone},
		{"ana", workAccess.KindCycle, cycle, accessBusiness.LevelFull},
		{"ben", workAccess.KindCycle, cycle, accessBusiness.LevelView},
		{"cy", workAccess.KindCycle, cycle, accessBusiness.LevelNone},
		{"ben", workAccess.KindForm, form, accessBusiness.LevelView},
	} {
		if d, err := accessBusiness.Rules.Resolve(ctx, w.subject(c.who), accessBusiness.Resource{Kind: c.kind, UUID: c.id.String()}); err != nil || d.Level != c.level {
			t.Errorf("%s on the %s: %+v %v, want %s", c.who, c.kind, d, err, c.level)
		}
	}

	// The Task Writer: someone outside the project changes nothing, and
	// nothing is made for them; a member's change is made, as every task
	// route makes it since A1; so is an admin's.
	ben, ana, cy := w.account("ben"), w.account("ana"), w.account("cy")
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &cy, uuid.MustParse(w.task), done, 0); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("a change by someone outside the project: %v", err)
	}
	inProgress := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"inProgress"`)}}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ben, uuid.MustParse(w.task), inProgress, 0); err != nil {
		t.Errorf("a member's change: %v", err)
	}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ana, uuid.MustParse(w.task), done, 0); err != nil {
		t.Fatalf("the admin's change: %v", err)
	}
	got, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil || got.Status != "done" {
		t.Errorf("the task's status after the admin's change: %+v %v", got, err)
	}

	// A policy's value for one project is kept for that project only.
	p, q := uuid.MustParse(w.project), uuid.New()
	if err := configModels.SetScopedValue(ctx, "project", p, "projects.edit.who", "members", ana.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if err := configModels.SetScopedValue(ctx, "project", p, "projects.edit.who", "assigned", ana.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if mine, err := configModels.ScopedValues(ctx, "project", p); err != nil || mine["projects.edit.who"] != "assigned" || len(mine) != 1 {
		t.Errorf("the project's own: %v %v", mine, err)
	}
	if other, err := configModels.ScopedValues(ctx, "project", q); err != nil || len(other) != 0 {
		t.Errorf("another project's: %v %v", other, err)
	}
	if err := configModels.ClearScopedValue(ctx, "project", p, "projects.edit.who"); err != nil {
		t.Fatal(err)
	}
	if mine, _ := configModels.ScopedValues(ctx, "project", p); len(mine) != 0 {
		t.Errorf("cleared, still %v", mine)
	}
}

// TaskFor reads a restricted task as the person it's asked about, whoever the
// context carries. A job or an approval moving a task for someone has no one
// signed in, or someone else: before, a member the task was kept to was told
// "task not found" with no one signed in, and one it was kept from read it
// through someone who may.
func TestARestrictedTaskIsReadAsThePersonAskedAbout(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	// mine (assigned to ben) is kept to ben; task (ana's) to its admins and
	// its maker, so not to ben, a member.
	for _, kept := range []struct {
		id     string
		people []uuid.UUID
	}{{w.mine, []uuid.UUID{ben.UserPostgresInfo.Id}}, {w.task, nil}} {
		task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, kept.id, ana.UserDgraphInfo.Uid)
		if err != nil {
			t.Fatal(err)
		}
		if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, kept.people, time.Now()); err != nil {
			t.Fatalf("restrict: %v", err)
		}
	}
	mine, task := uuid.MustParse(w.mine), uuid.MustParse(w.task)

	if got, _, err := taskBusiness.TaskFor(ctx, mine, &ben, workAccess.NeedEditTask); err != nil || got == nil {
		t.Errorf("ben, whom it's kept to, with no one signed in: %v", err)
	}
	asAna := taskread.ActingFor(ctx, ana.UserPostgresInfo.Id.String())
	if got, _, err := taskBusiness.TaskFor(asAna, task, &ben, workAccess.NeedView); !errors.Is(err, taskBusiness.ErrTaskNotVisible) || got != nil {
		t.Errorf("ben, whom it's kept from, read through ana: %v", err)
	}
	// A read for anyone's eyes still leaves every restricted task out.
	if _, _, err := taskBusiness.TaskFor(taskread.AsNobody(ctx), mine, &ben, workAccess.NeedView); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("ben's task, read for no one in particular: %v", err)
	}
}

// workWorld is a workspace with one project of a team, its people and its
// tasks, in Postgres and Dgraph:
//   - ana made the project and is its admin;
//   - ben is its member;
//   - cy is in the team but not the project;
//   - dee is the team's admin, not in the project;
//   - eve is outside both, and the assignee of one of its tasks;
//   - fay is on the project's admins without being on its members;
//   - wes is one of the workspace's admins, in no team.
type workWorld struct {
	t       *testing.T
	env     *integration.Env
	dg      *integration.DgraphEnv
	people  map[string]uuid.UUID
	team    string
	project string
	// task is ana's; assigned is ben's, assigned to eve; mine is ana's,
	// assigned to ben; deleted is in the trash.
	task, assigned, mine, deleted string
}

func seedWorkWorld(t *testing.T) *workWorld {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	w := &workWorld{t: t, env: env, dg: dg, people: map[string]uuid.UUID{}}
	live := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, name := range []string{"ana", "ben", "cy", "dee", "eve", "fay", "wes"} {
		id := uuid.New()
		w.people[name] = id
		w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	w.team, w.project = uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO admin_users (email_id) VALUES ('wes@acme.test')`)
	w.task, w.assigned, w.mine, w.deleted = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Web', $2)`, w.team, w.people["dee"])
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Website', $2, $3)`, w.project, w.team, w.people["ana"])
	for _, task := range []string{w.task, w.assigned, w.mine, w.deleted} {
		w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, w.project, w.people["ana"])
	}
	gone := time.Now().UTC().Format(time.RFC3339)
	tasks := []map[string]any{
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": w.task, "task_name": "Write the launch post", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ana"}},
		{"uid": "_:assigned", "dgraph.type": "Task", "task_uuid": w.assigned, "task_name": "Check the copy", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ben"},
			"task_assignee": map[string]any{"uid": "_:eve"}},
		{"uid": "_:mine", "dgraph.type": "Task", "task_uuid": w.mine, "task_name": "Send the newsletter", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ana"},
			"task_assignee": map[string]any{"uid": "_:ben"}},
		{"uid": "_:deleted", "dgraph.type": "Task", "task_uuid": w.deleted, "task_name": "Old idea", "task_status": "todo",
			"task_deleted_at": gone, "task_project": map[string]any{"uid": "_:website"}, "task_created_by": map[string]any{"uid": "_:ben"}},
	}
	// The task assigned to ben is on his list (My Tasks).
	nodes = append(nodes, map[string]any{"uid": "_:ben", "user_tasks": []map[string]any{{"uid": "_:mine"}}})
	nodes = append(nodes, map[string]any{"uid": "_:website", "dgraph.type": "Project", "project_uuid": w.project, "project_name": "Website",
		"project_deleted_at": live, "project_members": in("ana", "ben"), "project_admins": in("ana", "fay"),
		"project_created_by": map[string]any{"uid": "_:ana"}, "project_tasks": tasks,
		"project_team": map[string]any{"uid": "_:web", "dgraph.type": "Team", "team_uuid": w.team, "team_name": "Web",
			"team_members": in("ana", "ben", "cy", "dee"), "team_admins": in("dee"),
			// The team lists its project, as making one in it writes.
			"team_projects": []map[string]any{{"uid": "_:website"}}}})
	dg.Mutate(t, nodes)
	return w
}

func (w *workWorld) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.env.PG.Exec(q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// account is the person as the auth middleware puts them on a request.
func (w *workWorld) account(name string) userModels.UserInfo {
	w.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		w.t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		w.t.Fatalf("load %s's node: %v", name, err)
	}
	return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
}

// signedIn is the person's request context as the middleware leaves it, and
// the person. Reads through it see the restricted tasks they're let into
// (models/taskread); a bare context reads as nobody, who sees none.
func (w *workWorld) signedIn(name string) (context.Context, userModels.UserInfo) {
	u := w.account(name)
	return context.WithValue(taskread.Install(context.Background()), helpers.UserInfoContextKey, u), u
}

// subject is the person as the Access Resolver names them.
func (w *workWorld) subject(name string) accessBusiness.Subject {
	u := w.account(name)
	return accessBusiness.SubjectOf(&u)
}
