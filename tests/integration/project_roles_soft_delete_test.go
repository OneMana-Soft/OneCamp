//go:build integration
// +build integration

package integration_test

// A project role taken away is kept, with who took it and when (V-184 head 3,
// the owner's rule that every delete of a business record is soft).
//
// project_roles says how much each person may do in a project. Taking someone
// off the project, taking them off the team whose project it is, and settling
// a project's roles against its members each DELETEd the row, so nobody could
// say afterwards who had held which role, or who took it away. Now the row
// stays, marked deleted by whoever took it (nobody, for the settling job), no
// read sees it, and granting the role again brings the same row back.
//
// Run: go test -tags=integration ./tests/integration/ -run TestATakenProjectRoleIsKeptWithWhoAndWhen -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATakenProjectRoleIsKeptWithWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Owner made "Budget 2027" and runs it; Maya, Ben and Sam are in it. It is
	// the Finance team's project: Tara runs the team, and Sam is in it too.
	// Gus once held a role here but is in neither any more.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"owner", "maya", "ben", "sam", "tara", "gus"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	project, team := uuid.New(), uuid.New()
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
	nodes = append(nodes, map[string]any{"uid": "_:budget", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Budget 2027",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": in("owner", "maya", "ben", "sam"), "project_admins": in("owner"),
		"project_created_by": map[string]any{"uid": "_:owner"},
		"project_team": map[string]any{"uid": "_:finance", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Finance",
			"team_members": in("owner", "tara", "sam"), "team_admins": in("tara"), "team_projects": in("budget")}})
	uids := dg.Mutate(t, nodes)

	// Everyone's role, as the project's admins set them.
	for name, role := range map[string]string{"owner": "admin", "maya": "commenter", "ben": "viewer", "sam": "member", "gus": "member"} {
		if _, err := env.PG.Exec(`INSERT INTO project_roles (project_id, user_id, role, granted_by) VALUES ($1, $2, $3, $4)`,
			project, people[name], role, people["owner"]); err != nil {
			t.Fatal(err)
		}
	}

	// as is the request context of who, signed in.
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
	post := func(h http.HandlerFunc, who string, body map[string]string) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
		return rec.Code
	}

	// live is whether any read of the roles still sees whom's role: the
	// project's, the person's, many projects', and many people's.
	live := func(whom string) []string {
		t.Helper()
		var seen []string
		if all, err := roleModel.Roles(ctx, project); err != nil {
			t.Fatalf("Roles: %v", err)
		} else if _, ok := all[people[whom]]; ok {
			seen = append(seen, "Roles")
		}
		if mine, err := roleModel.RolesIn(ctx, people[whom], []uuid.UUID{project}); err != nil {
			t.Fatalf("RolesIn: %v", err)
		} else if _, ok := mine[project]; ok {
			seen = append(seen, "RolesIn")
		}
		if many, err := roleModel.RolesOfMany(ctx, []uuid.UUID{project}); err != nil {
			t.Fatalf("RolesOfMany: %v", err)
		} else if _, ok := many[project][people[whom]]; ok {
			seen = append(seen, "RolesOfMany")
		}
		if among, err := roleModel.RolesAmong(ctx, []uuid.UUID{people[whom]}, []uuid.UUID{project}); err != nil {
			t.Fatalf("RolesAmong: %v", err)
		} else if _, ok := among[people[whom]][project]; ok {
			seen = append(seen, "RolesAmong")
		}
		return seen
	}
	// kept checks whom's role row is still there, marked deleted by by (nobody
	// when by is ""), and seen by no read.
	kept := func(step, whom, by string) {
		t.Helper()
		var rows int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM project_roles WHERE project_id = $1 AND user_id = $2`, project, people[whom]).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Errorf("%s: %s's role row is gone (%d rows): a role taken away must be kept", step, whom, rows)
			return
		}
		var takenBy uuid.NullUUID
		var marked bool
		if err := env.PG.QueryRow(`SELECT deleted_by, deleted_at IS NOT NULL FROM project_roles WHERE project_id = $1 AND user_id = $2`,
			project, people[whom]).Scan(&takenBy, &marked); err != nil {
			t.Errorf("%s: read who took %s's role: %v", step, whom, err)
			return
		}
		want := uuid.NullUUID{}
		if by != "" {
			want = uuid.NullUUID{UUID: people[by], Valid: true}
		}
		if !marked || takenBy != want {
			t.Errorf("%s: %s's role row is marked deleted %v by %v, want deleted by %s", step, whom, marked, takenBy, by)
		}
		if seen := live(whom); len(seen) > 0 {
			t.Errorf("%s: %v still see %s's role", step, seen, whom)
		}
	}

	// The project's admin takes Maya off the project.
	if code := post(projectController.RemoveMemberFromProject, "owner", map[string]string{"project_uuid": project.String(), "user_uuid": people["maya"].String()}); code != http.StatusOK {
		t.Fatalf("owner taking maya off: %d", code)
	}
	kept("taken off the project", "maya", "owner")

	// The team's admin takes Sam off the team, and so off its projects.
	if code := post(teamController.RemoveMemberFromTeam, "tara", map[string]string{"team_uuid": team.String(), "member_uuid": people["sam"].String()}); code != http.StatusOK {
		t.Fatalf("tara taking sam off the team: %d", code)
	}
	kept("taken off the team", "sam", "tara")

	// Settling the roles against the members takes Gus's: nobody did it.
	if _, err := projectBusiness.SettleRoles(ctx); err != nil {
		t.Fatalf("SettleRoles: %v", err)
	}
	kept("settled", "gus", "")
	if seen := live("ben"); len(seen) != 4 {
		t.Errorf("settling left ben's role seen by %v, want every read", seen)
	}

	// Granting Maya her old role again brings the same row back, live, with
	// nobody's deletion on it.
	if err := roleModel.Set(ctx, project, people["maya"], "commenter", people["owner"]); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if seen := live("maya"); len(seen) != 4 {
		t.Errorf("maya's role granted again is seen by %v, want every read", seen)
	}
	var rows int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM project_roles WHERE project_id = $1 AND user_id = $2 AND role = 'commenter'`, project, people["maya"]).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("maya's role granted again: %d commenter rows (%v), want 1", rows, err)
	}

	// Gus is put back on the project by the graph alone, which writes no role
	// row, at the role his taken row still says. Settling brings that same row
	// back: one row, live, at the graph's role, granted by nobody and with no
	// deletion on it (CODE-REVIEWER-153, row 2240: Keep's revival clause).
	dg.Mutate(t, map[string]any{"uid": uids["budget"], "project_members": []map[string]any{{"uid": uids["gus"]}}})
	if _, err := projectBusiness.SettleRoles(ctx); err != nil {
		t.Fatalf("SettleRoles with gus back on the graph: %v", err)
	}
	var role string
	var grantedBy, deletedBy uuid.NullUUID
	var deleted bool
	if err := env.PG.QueryRow(`SELECT COUNT(*) OVER (), role, granted_by, deleted_at IS NOT NULL, deleted_by FROM project_roles WHERE project_id = $1 AND user_id = $2`,
		project, people["gus"]).Scan(&rows, &role, &grantedBy, &deleted, &deletedBy); err != nil {
		t.Fatalf("gus's role row, settled back: %v", err)
	}
	if rows != 1 || role != roleModel.Member || grantedBy.Valid || deleted || deletedBy.Valid {
		t.Errorf("gus settled back: %d rows, role %q, granted by %v, deleted %v by %v; want his one row, live, %s, granted and deleted by nobody",
			rows, role, grantedBy, deleted, deletedBy, roleModel.Member)
	}
	if seen := live("gus"); len(seen) != 4 {
		t.Errorf("gus settled back is seen by %v, want every read", seen)
	}
}
