//go:build integration
// +build integration

package integration_test

// Only a project's admins and its team's admins take people off a project,
// and anyone may leave it (PROJ-BUG-5).
//
// POST /project/removeMember checked only who was being removed: Mallory, in
// no team and no project, took Maya off "Budget 2027", and she lost its
// tasks, its search results and its notifications.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOnlyAdminsTakePeopleOffAProject -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyAdminsTakePeopleOffAProject(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Owner made the project and runs it; Maya, Ben and Sam are in it; Tara
	// runs the team and isn't in it; Mallory is in neither.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"owner", "maya", "ben", "sam", "tara", "mallory"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	project := uuid.NewString()
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	nodes = append(nodes, map[string]any{"uid": "_:budget", "dgraph.type": "Project", "project_uuid": project, "project_name": "Budget 2027",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": in("owner", "maya", "ben", "sam"), "project_admins": in("owner"),
		"project_created_by": map[string]any{"uid": "_:owner"},
		"project_team": map[string]any{"dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Finance",
			"team_members": in("owner", "tara"), "team_admins": in("tara")}})
	dg.Mutate(t, nodes)

	// remove sends what the web sends to take whom off the project (the
	// project's and the person's ids, as JSON) as who, and returns the status
	// and the answer's msg: the web shows a refusal's msg as it is, under its
	// toast's title, so a refusal must say why there.
	remove := func(who, whom, from string) (int, string) {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[who])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[who].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", who, err)
		}
		target := uuid.NewString()
		if id, ok := people[whom]; ok {
			target = id.String()
		}
		raw, _ := json.Marshal(map[string]string{"project_uuid": from, "user_uuid": target})
		rec := httptest.NewRecorder()
		projectController.RemoveMemberFromProject(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(
			context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})))
		var answer struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Errorf("%s taking %s off: the answer isn't JSON (%v): %s", who, whom, err, rec.Body.String())
		}
		return rec.Code, answer.Msg
	}
	members := func() map[string]bool {
		t.Helper()
		var out struct {
			P []struct {
				Members []struct {
					Name string `json:"user_name"`
				} `json:"project_members"`
			} `json:"p"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ p(func: eq(project_uuid, "`+project+`")) { project_members { user_name } } }`), &out)
		got := map[string]bool{}
		for _, p := range out.P {
			for _, m := range p.Members {
				got[m.Name] = true
			}
		}
		return got
	}

	// says is the msg a refusal must carry, where it is certain; "" checks none.
	for _, c := range []struct {
		name, who, whom, from string
		want                  int
		stillIn               bool
		says                  string
	}{
		{"someone in no team and no project", "mallory", "maya", project, http.StatusNotFound, true, "Project not found"},
		{"a member who isn't an admin", "ben", "maya", project, http.StatusForbidden, true, "Only the project's admins can take people off it."},
		{"the team's admin, outside the project", "tara", "maya", project, http.StatusOK, false, ""},
		{"the project's admin", "owner", "sam", project, http.StatusOK, false, ""},
		{"a member leaving", "ben", "ben", project, http.StatusOK, false, ""},
		{"anyone, for the project's creator", "tara", "owner", project, http.StatusForbidden, true, "The project's creator can't be taken off it."},
		{"an admin, for someone not in it", "owner", "maya", project, http.StatusNotFound, false, "They aren't in this project."},
		{"an admin, for someone who isn't anyone", "owner", "nobody", project, http.StatusNotFound, false, "They aren't in this project."},
		{"anyone, for a project that isn't there", "owner", "owner", uuid.NewString(), http.StatusNotFound, true, "Project not found"},
	} {
		if got, said := remove(c.who, c.whom, c.from); got != c.want || (c.says != "" && said != c.says) {
			t.Errorf("%s taking %s off: %d %q, want %d %q", c.name, c.whom, got, said, c.want, c.says)
		}
		if _, known := people[c.whom]; known && members()[c.whom] != c.stillIn {
			t.Errorf("after %s taking %s off, %s is in the project: %v", c.name, c.whom, c.whom, !c.stillIn)
		}
	}

	// Each removal is in the audit log.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'project.member.remove'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log has %d removals, want 3", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
