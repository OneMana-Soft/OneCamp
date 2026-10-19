//go:build integration

package business

// Whether a public form is open, against a real Postgres 12 and Dgraph.
// Run: go test -tags=integration ./business/Form/ -run TestAFormClosesWhenItsOwnerLeaves -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Formula"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
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
	f, err := formModel.Create(formModel.Form{ProjectUUID: project, Token: "abcdefghijklmnopqrstuvwx", Title: "Report a bug",
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
	if err := Submit(ctx, f.Token, map[string]any{"f1": "It broke"}, formula.NumberSigns{}, time.Now()); !errors.Is(err, ErrNoForm) {
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
	saved, err := Save(project, Input{Id: f.Id.String(), Title: "Report a bug", Priority: "medium", Active: true,
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
	f, err := formModel.Create(formModel.Form{ProjectUUID: project, Token: "bcdefghijklmnopqrstuvwxy", Title: "Report a bug",
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
