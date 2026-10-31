//go:build integration

package business

// Whether a public form is open, against a real Postgres 12 and Dgraph.
// Run: go test -tags=integration ./business/Form/ -run TestAFormClosesWhenItsOwnerLeaves -v

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Formula"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// A form files tasks as the person who made it. Once they've left the
// project, it answers as no form: its tasks (and the notifications they bring
// their filer) would otherwise go on reaching someone outside the project.
func TestAFormClosesWhenItsOwnerLeaves(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)

	owner, project := uuid.New(), uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		owner, owner.String()+"@example.test", "o-"+owner.String()[:8], "Owner"); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "Owner", "dgraph.type": "User"},
		{"uid": "_:team", "team_uuid": uuid.NewString(), "team_name": "Team", "dgraph.type": "Team"},
		{"uid": "_:project", "project_uuid": project.String(), "project_name": "Intake", "dgraph.type": "Project",
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:owner"}}},
	})

	fields, _ := json.Marshal([]Field{{Id: "f1", Label: "What happened?", Type: "text", Required: true}})
	f, err := formModel.Create(ctx, formModel.Form{ProjectUUID: project, Token: "abcdefghijklmnopqrstuvwx", Title: "Report a bug",
		Fields: fields, Priority: "medium", Active: true, CreatedBy: owner})
	if err != nil {
		t.Fatalf("create the form: %v", err)
	}

	if _, err := GetPublic(ctx, f.Token); err != nil {
		t.Fatalf("the form while its owner is in the project: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{"uid": uids["project"], "project_members": []map[string]any{{"uid": uids["owner"]}}})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DeleteJson: raw, CommitNow: true}); err != nil {
		t.Fatalf("take the owner out of the project: %v", err)
	}
	if _, err := GetPublic(ctx, f.Token); !errors.Is(err, ErrNoForm) {
		t.Errorf("the form after its owner left the project: %v, want ErrNoForm", err)
	}
	// The signs a visitor who sends none is read in, as SubmitPublicForm
	// passes them: the answer here is words.
	if err := Submit(ctx, f.Token, map[string]any{"f1": "It broke"}, formula.NumberSigns{}, "", "", time.Now()); !errors.Is(err, ErrNoForm) {
		t.Errorf("an answer after its owner left: %v, want ErrNoForm", err)
	}

	// An admin still in the project saves it: it files as them, and is open again.
	admin := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		admin, admin.String()+"@example.test", "a-"+admin.String()[:8], "Admin"); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:admin", "user_uuid": admin.String(), "user_name": "Admin", "dgraph.type": "User"},
		{"uid": uids["project"], "project_members": []map[string]any{{"uid": "_:admin"}}},
	})
	saved, err := Save(ctx, project, Input{Id: f.Id.String(), Title: "Report a bug", Priority: "medium", Active: true,
		Fields: []Field{{Id: "f1", Label: "What happened?", Type: ShortText, Required: true}}}, admin)
	if err != nil || saved == nil || saved.CreatedBy != admin {
		t.Fatalf("an admin saving the form: %+v %v", saved, err)
	}
	if _, err := GetPublic(ctx, f.Token); err != nil {
		t.Errorf("the form after an admin in the project saved it: %v", err)
	}
}

// Its tasks are filed as its owner, so a form is open only while its owner may
// add tasks to the project (Work Access, A1): made a viewer, or a member once
// the project keeps its tasks with its admins, its owner closes it.
func TestAFormClosesWhenItsOwnerMayNoLongerAddTasks(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)

	owner, project := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		owner, owner.String()+"@example.test", "o-"+owner.String()[:8], "Owner")
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Intake', $2)`, project, owner)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "Owner", "dgraph.type": "User"},
		{"uid": "_:team", "team_uuid": uuid.NewString(), "team_name": "Team", "dgraph.type": "Team"},
		{"uid": "_:project", "project_uuid": project.String(), "project_name": "Intake", "dgraph.type": "Project",
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:owner"}}},
	})
	fields, _ := json.Marshal([]Field{{Id: "f1", Label: "What happened?", Type: "text", Required: true}})
	f, err := formModel.Create(ctx, formModel.Form{ProjectUUID: project, Token: "bcdefghijklmnopqrstuvwxy", Title: "Report a bug",
		Fields: fields, Priority: "medium", Active: true, CreatedBy: owner})
	if err != nil {
		t.Fatalf("create the form: %v", err)
	}
	open := func(what string, want bool) {
		t.Helper()
		_, err := GetPublic(ctx, f.Token)
		switch {
		case want && err != nil:
			t.Errorf("%s: %v, want it open", what, err)
		case !want && !errors.Is(err, ErrNoForm):
			t.Errorf("%s: %v, want ErrNoForm", what, err)
		}
	}

	open("its owner a member", true)
	exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, project, owner)
	open("its owner a viewer", false)
	exec(`UPDATE project_roles SET role = 'member' WHERE project_id = $1 AND user_id = $2`, project, owner)
	open("its owner a member again", true)

	by := policyBusiness.Changer{UserID: owner, Email: owner.String() + "@example.test"}
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: project.String()}
	if _, err := policyBusiness.Admin.SetIn(ctx, policyBusiness.ProjectsEditWho.Key, scope, json.RawMessage(`"admins"`), by); err != nil {
		t.Fatal(err)
	}
	open("its owner a member of a project that keeps tasks with its admins", false)
	if _, err := policyBusiness.Admin.ClearIn(ctx, policyBusiness.ProjectsEditWho.Key, scope, by); err != nil {
		t.Fatal(err)
	}
	open("the project back to the workspace's rule", true)
}

// A form that branches and fills its task, end to end against Postgres 12 and
// Dgraph: a bug that skips its steps is refused, one that gives them makes a
// task with the priority, due date and tags its answers set and the project
// field its answer fills, the submission is kept by question id, and the
// rules hear of it once, with its answers.
//
// Run: go test -tags=integration ./business/Form/ -run TestAFormThatBranchesFillsItsTask -v
func TestAFormThatBranchesFillsItsTask(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)
	owner, project := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		owner, owner.String()+"@example.test", "o-"+owner.String()[:8], "Owner")
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Intake', $2)`, project, owner)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "Owner", "dgraph.type": "User"},
		{"uid": "_:team", "team_uuid": uuid.NewString(), "team_name": "Team", "dgraph.type": "Team"},
		{"uid": "_:project", "project_uuid": project.String(), "project_name": "Intake", "dgraph.type": "Project",
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:owner"}}},
	})
	stage, err := taskFieldBusiness.Create(ctx, project, taskFieldBusiness.Input{Name: "Stage", Type: "select",
		Options: []taskFieldBusiness.OptionInput{{Label: "Draft"}, {Label: "Live"}}}, owner, "USD")
	if err != nil {
		t.Fatalf("make the project's field: %v", err)
	}
	told := make(chan map[string]interface{}, 4)
	oldTell := tellRules
	t.Cleanup(func() { tellRules = oldTell })
	tellRules = func(_ context.Context, data map[string]interface{}) { told <- data }

	f, err := Save(ctx, project, Input{Title: "Requests", Priority: "medium", Active: true, TitleField: "what",
		Confirmation: "Thanks, it's on our board.",
		Fields: []Field{
			{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
			{Id: "kind", Label: "Kind", Type: Select, Options: []string{"Bug", "Idea"}, MapsTo: MapTags},
			{Id: "steps", Label: "Steps", Type: LongText, Required: true, ShowIf: &ShowIf{Question: "kind", Op: ShowIs, Values: []string{"Bug"}}},
			{Id: "soon", Label: "How soon?", Type: Select, Options: []string{"Now", "Later"}, MapsTo: MapPriority, Map: map[string]string{"Now": "high", "Later": "low"}},
			{Id: "by", Label: "Needed by", Type: Date, MapsTo: MapDue},
			{Id: "stage", Label: "Stage", Type: Select, Options: []string{"ignored", "the field's own"}, MapsTo: MapField + stage.ID.String()},
		}}, owner)
	if err != nil {
		t.Fatalf("save the form: %v", err)
	}
	var stored []Field
	_ = json.Unmarshal(f.Fields, &stored)
	if got := strings.Join(stored[5].Options, ","); got != "Draft,Live" {
		t.Errorf("the field's question offers %q, not the field's options", got)
	}
	pub, err := GetPublic(ctx, f.Token)
	if err != nil || pub.Confirmation != "Thanks, it's on our board." || pub.Fields[1].MapsTo != "" || pub.Fields[3].Map != nil {
		t.Fatalf("public form %+v %v: it shows what its answers fill, or loses its message", pub, err)
	}

	// Needed by a day ten days on, as the asker's zone writes it.
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	by := time.Now().In(kolkata).AddDate(0, 0, 10)
	answers := map[string]any{"what": "Checkout crashes", "kind": "Bug", "soon": "Now", "by": by.Format(time.DateOnly), "stage": "Live"}
	if err := Submit(ctx, f.Token, answers, formula.NumberSigns{}, "Asia/Kolkata", "", time.Now()); err == nil {
		t.Fatal("a bug without its steps was taken")
	}
	answers["steps"] = "Pay with a saved card"
	if err := Submit(ctx, f.Token, answers, formula.NumberSigns{}, "Asia/Kolkata", "", time.Now()); err != nil {
		t.Fatalf("send the form: %v", err)
	}
	var ev map[string]interface{}
	select {
	case ev = <-told:
	case <-time.After(5 * time.Second):
		t.Fatal("the rules never heard of the form")
	}
	select {
	case again := <-told:
		t.Fatalf("the rules heard of one form twice: %v", again)
	case <-time.After(200 * time.Millisecond):
	}
	if ev["form_id"] != f.Id.String() || ev["event_id"] == "" || ev["project_name"] != "Intake" ||
		ev["answers"].(map[string]interface{})["steps"] != "Pay with a saved card" {
		t.Errorf("the rules heard %v", ev)
	}
	taskID := ev["task_id"].(string)

	q := `query t($id: string) { t(func: eq(task_uuid, $id)) { task_name task_priority task_due_date task_label } }`
	res, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, q, map[string]string{"$id": taskID})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		T []struct {
			Name     string    `json:"task_name"`
			Priority string    `json:"task_priority"`
			Due      time.Time `json:"task_due_date"`
			Label    string    `json:"task_label"`
		} `json:"t"`
	}
	if err := json.Unmarshal(res.Json, &got); err != nil || len(got.T) != 1 {
		t.Fatalf("the task: %s %v", res.Json, err)
	}
	task := got.T[0]
	if task.Name != "Checkout crashes" || task.Priority != "high" || task.Label != "Bug" || !task.Due.Equal(time.Date(by.Year(), by.Month(), by.Day(), 0, 0, 0, 0, kolkata)) {
		t.Errorf("the task is %+v", task)
	}
	var value string
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT value::text FROM task_field_values WHERE task_uuid = $1 AND field_id = $2`, taskID, stage.ID).Scan(&value); err != nil {
		t.Fatalf("the task's Stage: %v", err)
	}
	if want, _ := json.Marshal(stage.Options[1].ID); value != string(want) {
		t.Errorf("Stage is %s, want Live (%s)", value, want)
	}
	subs, err := formModel.Latest(ctx, f.Id, 50)
	if err != nil || len(subs) != 1 {
		t.Fatalf("%d submissions, %v", len(subs), err)
	}
	var kept []Answer
	if err := json.Unmarshal(subs[0].Answers, &kept); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, a := range kept {
		byID[a.Id] = a.Value
	}
	if byID["steps"] != "Pay with a saved card" || byID["soon"] != "Now" {
		t.Errorf("the submission keeps %s", subs[0].Answers)
	}
}
