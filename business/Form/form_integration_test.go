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
	if err := Submit(ctx, f.Token, map[string]any{"f1": "It broke"}, time.Now()); !errors.Is(err, ErrNoForm) {
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
