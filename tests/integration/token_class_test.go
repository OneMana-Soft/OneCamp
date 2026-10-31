//go:build integration
// +build integration

package integration_test

// An API token acts at most as a member everywhere a token reaches, against
// real Postgres, Dgraph and Redis, through the router's public API.
//
// The owner's decision of 16 Oct 2026: a token carries none of its owner's admin
// powers; what an admin may do, they do in the app, as themselves. /v1's tables
// took the owner's workspace-admin flag (an admin's token listed, opened and
// wrote every table), and an announcement channel's admin posted in it through
// a token.
//
// This edition's form of the AI edition's mcp_token_class_test.go: the same
// test, less its legs for MCP, the AI tools and agent jobs, which this edition
// doesn't have.
//
// Run: go test -tags=integration ./tests/integration/ -run TestATokenActsAsAMemberAcrossTheAPI -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATokenActsAsAMemberAcrossTheAPI(t *testing.T) {
	t.Setenv("JWT_SECRET", "token-class-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	// Posts are indexed in the background; searches find nothing.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"timed_out":false,"hits":{"total":{"value":0,"relation":"eq"},"hits":[]},"responses":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)

	person := func(email, name string) userModels.UserInfo {
		t.Helper()
		id, _, err := userBusiness.CreateUserWithMethod(ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || node == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
	}
	// Ana is a workspace admin, the admin of #announcements and of the Platform
	// team. Bo is a member, with a project of his own. Cy is in the Platform team.
	ana := person("ana@example.test", "Ana")
	if err := userBusiness.CreateAdminUser(ctx, "ana@example.test"); err != nil {
		t.Fatal(err)
	}
	ana.UserPostgresInfo.IsAdmin = true
	bo := person("bo@example.test", "Bo")
	cy := person("cy@example.test", "Cy")

	// Bo's tables: one only he sees, one the whole workspace sees.
	boActor := dataTableBusiness.Actor{UserID: bo.UserPostgresInfo.Id}
	salaries, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Salaries", Visibility: "private"}, boActor)
	if err != nil {
		t.Fatalf("make Bo's private table: %v", err)
	}
	roadmap, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Roadmap", Visibility: "workspace"}, boActor)
	if err != nil {
		t.Fatalf("make Bo's workspace table: %v", err)
	}

	// #announcements: Ana made it, so she is its admin; only its admins post.
	err, announcements := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "announcements"}, &ana)
	if err != nil {
		t.Fatalf("make #announcements: %v", err)
	}
	if err := channelBusiness.SetChannelPostPolicy(ctx, announcements, channelBusiness.ChannelPostPolicyAdminsOnly); err != nil {
		t.Fatalf("make #announcements admins-only: %v", err)
	}
	posts := func() (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, announcements).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The Platform team, with Ana its admin and Cy in it.
	team := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Platform', $2)`, team, ana.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{
		"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Platform",
		"team_deleted_at": "0001-01-01T00:00:00Z",
		"team_admins":     []map[string]any{{"uid": ana.UserDgraphInfo.Uid}},
		"team_members":    []map[string]any{{"uid": ana.UserDgraphInfo.Uid}, {"uid": cy.UserDgraphInfo.Uid}},
	}})
	// Bo's project, in a team Ana isn't in.
	website := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, website, bo.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": bo.UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": website.String(), "project_name": "Website",
			"project_deleted_at": "0001-01-01T00:00:00Z",
			"project_members":    []map[string]any{{"uid": bo.UserDgraphInfo.Uid}},
			"project_admins":     []map[string]any{{"uid": bo.UserDgraphInfo.Uid}},
			"project_team":       map[string]any{"dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Web"}},
	})

	// Ana's token, with every scope: what it may do is her reach, as a member.
	anaToken, err := apiTokenBusiness.CreateToken(ctx, "Ana's script", apiTokenBusiness.AllScopes, 30, ana.UserPostgresInfo.Id, nil, true)
	if err != nil {
		t.Fatalf("make Ana's token: %v", err)
	}

	routes := router.Routes()
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+anaToken.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	t.Run("over the public API, an admin's token lists, opens and adds to the tables a member does", func(t *testing.T) {
		code, out := call("GET", "/v1/tables", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/tables answered %d: %v", code, out)
		}
		listed := map[string]bool{}
		items, _ := out["data"].([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				name, _ := m["name"].(string)
				listed[name] = true
			}
		}
		if listed["Salaries"] {
			t.Error("GET /v1/tables lists Bo's private table to Ana's token: a workspace admin's power, through a token")
		}
		if !listed["Roadmap"] {
			t.Errorf("GET /v1/tables leaves out the table the whole workspace sees: %v", listed)
		}
		if code, out := call("GET", "/v1/tables/"+salaries.Id.String(), nil); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("GET /v1/tables/{Bo's private table} answered %d, want it refused: %v", code, out)
		}
		if code, out := call("POST", "/v1/tables/"+salaries.Id.String()+"/rows", map[string]any{"values": map[string]any{}}); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("POST a row into Bo's private table answered %d, want it refused: %v", code, out)
		}
		// A table the whole workspace sees takes anyone's rows, a token's too.
		if code, out := call("POST", "/v1/tables/"+roadmap.Id.String()+"/rows", map[string]any{"values": map[string]any{}}); code != http.StatusCreated {
			t.Errorf("POST a row into the workspace's table answered %d, want 201: %v", code, out)
		}
		// In the app, as herself, she is still the workspace's admin.
		if _, err := dataTableBusiness.GetBundle(ctx, salaries.Id, dataTableBusiness.Actor{UserID: ana.UserPostgresInfo.Id, IsAdmin: true}); err != nil {
			t.Errorf("in the app, the workspace admin can't open Bo's private table: %v", err)
		}
	})

	t.Run("an announcement channel's admin posts in it in the app, never through a token", func(t *testing.T) {
		if code, out := call("POST", "/v1/messages/channel", map[string]any{"channel_uuid": announcements.String(), "text": "Release at five"}); code != http.StatusForbidden {
			t.Errorf("POST /v1/messages/channel into #announcements answered %d, want 403: %v", code, out)
		}
		// (The AI edition also tries its send_message tool here, over MCP and as
		// an agent run a token asked for; this edition has neither.)
		if n := posts(); n != 0 {
			t.Errorf("#announcements has %d posts made through Ana's token, want none", n)
		}
		// In the app, as herself, she posts there.
		if _, err := sendBusiness.PrepareChannelPost(ctx, &ana, &postAdapter.InputCreateOrUpdatePostInfo{
			ChannelUuid: announcements.String(), HTMLText: "<p>Release at five</p>"}); err != nil {
			t.Errorf("in the app, the channel's admin can't post in #announcements: %v", err)
		}
	})

	// Who makes projects in a team is one rule, the workspace's "Who can make
	// projects" (workAccess.CanMakeProjects). This edition's public API makes no
	// projects, and it has no agents or MCP, so the app is its one path here.
	t.Run("who makes projects in a team is the workspace's rule, in the app", func(t *testing.T) {
		by := policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: ana.UserPostgresInfo.EmailID}
		t.Cleanup(func() {
			_, _ = policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsCreateWho.Key, json.RawMessage(`"team_admins"`), by)
		})
		made := 0
		name := func() string { made++; return fmt.Sprintf("Launch %d", made) }
		type maker struct {
			who   string
			info  userModels.UserInfo
			admin bool
		}
		makers := []maker{
			{"Ana, the team's admin", ana, true},
			{"Cy, one of its members", cy, false},
		}
		type path struct {
			name    string
			byToken bool
			makes   func(m maker) (bool, string)
		}
		paths := []path{
			{"in the app", false, func(m maker) (bool, string) {
				raw, _ := json.Marshal(map[string]string{"project_name": name(), "project_team_uuid": team})
				rec := httptest.NewRecorder()
				projectController.CreateProject(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
					WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, m.info)))
				return rec.Code >= 200 && rec.Code < 300, rec.Body.String()
			}},
			// (The AI edition also checks an agent asked in the app, MCP's reach,
			// an agent run a token asked for, and MCP through a token.)
		}
		for _, who := range []string{"team_admins", "team_members"} {
			if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsCreateWho.Key, json.RawMessage(`"`+who+`"`), by); err != nil {
				t.Fatalf("set who makes projects to %s: %v", who, err)
			}
			for _, p := range paths {
				for _, m := range makers {
					// Everyone in the team where its members may; else its admins,
					// as themselves, never through a token.
					want := who == "team_members" || (m.admin && !p.byToken)
					if got, why := p.makes(m); got != want {
						t.Errorf("%s makes projects: %s, %s: %v (%s), want %v", who, p.name, m.who, got, why, want)
					}
				}
			}
		}
	})

	// A token gets no workspace admin's way into a project its owner isn't in:
	// Work Access and ReaderIsIn read their member and admin edges only.
	t.Run("a workspace admin's token adds no task to a project its owner isn't in", func(t *testing.T) {
		if code, out := call("POST", "/v1/tasks", map[string]any{"task_name": "Ship it", "project_uuid": website.String()}); code != http.StatusNotFound {
			t.Errorf("POST /v1/tasks into Bo's project answered %d, want 404: %v", code, out)
		}
	})
}
