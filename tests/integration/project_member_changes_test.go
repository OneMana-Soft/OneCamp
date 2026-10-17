//go:build integration
// +build integration

package integration_test

// Adding someone to a project, making them an admin and unmaking one say
// who may (its admins and its team's admins), answer a project that isn't
// there as not found, and say what was done. They answered "Not Authorised"
// to every refusal and 400 to a project that isn't there, and unmaking an
// admin said "member removed from project!".
//
// Run: go test -tags=integration ./tests/integration/ -run TestEveryMemberChangeSaysWhoMayMakeIt -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEveryMemberChangeSaysWhoMayMakeIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Owner made the project and runs it; Ben is in it; Nia is in the team but
	// not the project, and so is Gil, a guest; Mallory is in neither.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"owner", "ben", "nia", "gil", "mallory"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": "0001-01-01T00:00:00Z", "is_external": name == "gil"})
	}
	project := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Budget 2027', $2)`, project, people["owner"]); err != nil {
		t.Fatal(err)
	}
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	nodes = append(nodes, map[string]any{"uid": "_:budget", "dgraph.type": "Project", "project_uuid": project, "project_name": "Budget 2027",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": in("owner", "ben"), "project_admins": in("owner"),
		"project_created_by": map[string]any{"uid": "_:owner"},
		"project_team": map[string]any{"dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Finance",
			"team_members": in("owner", "ben", "nia", "gil"), "team_admins": in("owner")}})
	dg.Mutate(t, nodes)

	as := func(who string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", who, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	call := func(h http.HandlerFunc, who, whom, in string) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"project_uuid": in, "user_uuid": people[whom].String()})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
		var body struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Msg
	}

	for _, c := range []struct {
		name      string
		h         http.HandlerFunc
		who, whom string
		project   string
		code      int
		msg       string
	}{
		{"a member adding someone", projectController.AddMemberToProject, "ben", "nia", project, http.StatusForbidden, "Only the project's admins can add people to it."},
		{"a member making an admin", projectController.AddAdminMemberToProject, "ben", "ben", project, http.StatusForbidden, "Only the project's admins can make someone an admin."},
		{"a member unmaking an admin", projectController.RemoveAdminRoleFromMember, "ben", "owner", project, http.StatusForbidden, "Only the project's admins can change who its admins are."},
		{"someone in neither, adding", projectController.AddMemberToProject, "mallory", "nia", project, http.StatusNotFound, "Project not found"},
		{"adding to a project that isn't there", projectController.AddMemberToProject, "owner", "nia", uuid.NewString(), http.StatusNotFound, "Project not found"},
		{"making an admin of a project that isn't there", projectController.AddAdminMemberToProject, "owner", "ben", uuid.NewString(), http.StatusNotFound, "Project not found"},
		{"unmaking an admin of a project that isn't there", projectController.RemoveAdminRoleFromMember, "owner", "ben", uuid.NewString(), http.StatusNotFound, "Project not found"},
		{"an admin adding someone outside the team", projectController.AddMemberToProject, "owner", "mallory", project, http.StatusNotFound, "They aren't in the project's team."},
		{"an admin adding a guest", projectController.AddMemberToProject, "owner", "gil", project, http.StatusBadRequest, "A guest can't join a project. Share it with them instead."},
		{"an admin adding someone in the team", projectController.AddMemberToProject, "owner", "nia", project, http.StatusOK, "Added to the project."},
		{"an admin making a member an admin", projectController.AddAdminMemberToProject, "owner", "ben", project, http.StatusOK, "Made an admin of the project."},
		{"an admin unmaking an admin", projectController.RemoveAdminRoleFromMember, "owner", "ben", project, http.StatusOK, "No longer an admin of the project."},
		{"a member unmaking the creator", projectController.RemoveAdminRoleFromMember, "ben", "owner", project, http.StatusForbidden, "Only the project's admins can change who its admins are."},
		{"the project's admin unmaking its creator", projectController.RemoveAdminRoleFromMember, "owner", "owner", project, http.StatusForbidden, "The project's creator stays one of its admins."},
	} {
		if code, msg := call(c.h, c.who, c.whom, c.project); code != c.code || msg != c.msg {
			t.Errorf("%s: %d %q, want %d %q", c.name, code, msg, c.code, c.msg)
		}
	}
}
