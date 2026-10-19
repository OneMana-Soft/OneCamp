//go:build integration
// +build integration

package integration_test

// An API token acts at most as a member everywhere a token reaches, against
// real Postgres, Dgraph and Redis, through the router: the public API, the MCP
// endpoint, the AI tools MCP and agent runs share, and an agent job a token
// queued, read back from Postgres the way the worker reads it.
//
// The owner's decision of 16 Oct 2026: a token carries none of its owner's admin
// powers; what an admin may do, they do in the app, as themselves. MCP's reach
// kept it from 0c95c3c3 on, and the rest of the class did not: /v1's tables
// took the owner's workspace-admin flag (an admin's token listed, opened and
// wrote every table), list_tables and list_data_sources listed every table and
// source, an announcement channel's admin posted in it through a token, and a
// job a token queued ran on the worker as its asker in the app. Who makes
// projects drifted too: the app and the agent's create_project asked the
// workspace's "Who can make projects" (a token as one of the team's members),
// while MCP's reach refused every token and asked everyone else to be a team
// admin, where the team's members may.
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
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	_ "github.com/akashc777/OneCamp/business/AI"
	aiAgentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	dataSourceModel "github.com/akashc777/OneCamp/models/postgres/DataSource"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	ai "github.com/akashc777/OneCamp/services/AI"
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
	anaID := ana.UserPostgresInfo.Id.String()

	if err := mcpBusiness.SetAdmission(ctx, true, "*"); err != nil {
		t.Fatalf("turn MCP on: %v", err)
	}
	if err := mcpBusiness.RegisterBridgedTools(); err != nil && !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("register the MCP tools: %v", err)
	}

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
	// Bo's data sources: one only he may query, one anyone may.
	source := func(name string, visibility string) {
		t.Helper()
		if _, err := dataSourceModel.Create(ctx, &dataSourceModel.DataSource{
			Name: name, Engine: dataSourceModel.EnginePostgres, Host: "warehouse.example.test", Port: 5432,
			Database: strings.ToLower(name), Username: "reader", SSLMode: dataSourceModel.SSLRequire,
			Visibility: visibility, Enabled: true, CreatedBy: bo.UserPostgresInfo.Id,
		}); err != nil {
			t.Fatalf("make Bo's %s source: %v", name, err)
		}
	}
	source("Finance", dataSourceModel.VisibilityPrivate)
	source("Metrics", dataSourceModel.VisibilityWorkspace)

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
	cyToken, err := apiTokenBusiness.CreateToken(ctx, "Cy's script", apiTokenBusiness.AllScopes, 30, cy.UserPostgresInfo.Id, nil, false)
	if err != nil {
		t.Fatalf("make Cy's token: %v", err)
	}
	// A call that came through her token, as the code behind /v1 sees it.
	viaToken := context.WithValue(ctx, helpers.ApiTokenIDContextKey, anaToken.Token.Id.String())

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
	// tool calls an MCP tool over /v1/mcp, and answers its text, or the
	// refusal's message.
	tool := func(name string, args map[string]any) (text string, refusal string) {
		t.Helper()
		code, out := call("POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		if code != http.StatusOK {
			t.Fatalf("%s answered %d: %v", name, code, out)
		}
		if e, ok := out["error"].(map[string]any); ok {
			msg, _ := e["message"].(string)
			return "", "refused: " + msg
		}
		res, _ := out["result"].(map[string]any)
		content, _ := res["content"].([]any)
		for _, c := range content {
			if m, ok := c.(map[string]any); ok {
				s, _ := m["text"].(string)
				text += s
			}
		}
		if isErr, _ := res["isError"].(bool); isErr {
			return "", "failed: " + text
		}
		return text, ""
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

	t.Run("over MCP, list_tables and list_data_sources list what a member sees", func(t *testing.T) {
		text, refusal := tool("list_tables", map[string]any{})
		if refusal != "" {
			t.Fatalf("list_tables: %s", refusal)
		}
		if strings.Contains(text, "Salaries") {
			t.Errorf("list_tables lists Bo's private table to Ana's token:\n%s", text)
		}
		if !strings.Contains(text, "Roadmap") {
			t.Errorf("list_tables leaves out the table the whole workspace sees:\n%s", text)
		}
		text, refusal = tool("list_data_sources", map[string]any{})
		if refusal != "" {
			t.Fatalf("list_data_sources: %s", refusal)
		}
		if strings.Contains(text, "Finance") {
			t.Errorf("list_data_sources lists Bo's private source to Ana's token:\n%s", text)
		}
		if !strings.Contains(text, "Metrics") {
			t.Errorf("list_data_sources leaves out the source anyone may query:\n%s", text)
		}
	})

	t.Run("an announcement channel's admin posts in it in the app, never through a token", func(t *testing.T) {
		if code, out := call("POST", "/v1/messages/channel", map[string]any{"channel_uuid": announcements.String(), "text": "Release at five"}); code != http.StatusForbidden {
			t.Errorf("POST /v1/messages/channel into #announcements answered %d, want 403: %v", code, out)
		}
		if _, refusal := tool("send_message", map[string]any{"channel_uuid": announcements.String(), "text": "Release at five"}); refusal == "" {
			t.Error("send_message over MCP posted in #announcements through Ana's token")
		}
		// The tool itself, as an agent run a token asked for reaches it.
		send, ok := ai.GetExecutor("send_message")
		if !ok {
			t.Fatal("no send_message tool")
		}
		if _, _, err := send(viaToken, ai.ProposedAction{ToolName: "send_message",
			Params: map[string]string{"channel_uuid": announcements.String(), "text": "Release at five"}}, anaID); err == nil {
			t.Error("the send_message tool posted in #announcements for a call made through a token")
		}
		if n := posts(); n != 0 {
			t.Errorf("#announcements has %d posts made through Ana's token, want none", n)
		}
		// In the app, as herself, she posts there; and an agent she asks there
		// reaches it as she does.
		if _, err := sendBusiness.PrepareChannelPost(ctx, &ana, &postAdapter.InputCreateOrUpdatePostInfo{
			ChannelUuid: announcements.String(), HTMLText: "<p>Release at five</p>"}); err != nil {
			t.Errorf("in the app, the channel's admin can't post in #announcements: %v", err)
		}
		ref := mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceChannel, ID: announcements.String(), Access: mcpBusiness.AccessWrite}
		if d := mcpBusiness.PrincipalCanReach(ctx, anaID, ref); !d.Allow {
			t.Errorf("Ana asking an agent in the app to post in #announcements: refused (%s)", d.Reason)
		}
	})

	// Who makes projects in a team is one rule, the workspace's "Who can make
	// projects" (workAccess.CanMakeProjects), on every path: the app, an agent
	// asked there, an agent run a token asked for, and MCP, through a token or
	// at the reach an agent asked in the app is held to. A token makes them as
	// one of the team's members, whatever its owner is.
	t.Run("who makes projects is one rule on every path, and a token makes them as one of the team's members", func(t *testing.T) {
		create, ok := ai.GetExecutor("create_project")
		if !ok {
			t.Fatal("no create_project tool")
		}
		by := policyBusiness.Changer{UserID: ana.UserPostgresInfo.Id, Email: ana.UserPostgresInfo.EmailID}
		t.Cleanup(func() {
			_, _ = policyBusiness.Admin.Set(ctx, policyBusiness.ProjectsCreateWho.Key, json.RawMessage(`"team_admins"`), by)
		})
		made := 0
		name := func() string { made++; return fmt.Sprintf("Launch %d", made) }
		type maker struct {
			who      string
			info     userModels.UserInfo
			token    string
			viaToken context.Context
			admin    bool
		}
		makers := []maker{
			{"Ana, the team's admin", ana, anaToken.Plaintext, viaToken, true},
			{"Cy, one of its members", cy, cyToken.Plaintext, context.WithValue(ctx, helpers.ApiTokenIDContextKey, cyToken.Token.Id.String()), false},
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
			{"an agent asked in the app", false, func(m maker) (bool, string) {
				_, _, err := create(ctx, ai.ProposedAction{ToolName: "create_project",
					Params: map[string]string{"team_uuid": team, "name": name()}}, m.info.UserPostgresInfo.Id.String())
				return err == nil, fmt.Sprint(err)
			}},
			{"MCP's reach, for an agent asked in the app", false, func(m maker) (bool, string) {
				d := mcpBusiness.PrincipalCanReach(ctx, m.info.UserPostgresInfo.Id.String(),
					mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceTeam, ID: team, Access: mcpBusiness.AccessWrite})
				return d.Allow, d.Reason
			}},
			{"an agent run a token asked for", true, func(m maker) (bool, string) {
				_, _, err := create(m.viaToken, ai.ProposedAction{ToolName: "create_project",
					Params: map[string]string{"team_uuid": team, "name": name()}}, m.info.UserPostgresInfo.Id.String())
				return err == nil, fmt.Sprint(err)
			}},
			{"MCP, through a token", true, func(m maker) (bool, string) {
				d := mcpBusiness.AuthorizeToolCall(ctx, mcpBusiness.ToolCallRequest{TokenPlaintext: m.token, ToolName: "create_project",
					Args: map[string]any{"team_uuid": team, "name": name()}})
				return d.Allow, d.Reason
			}},
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
		ref := mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceProject, ID: website.String(), Access: mcpBusiness.AccessWrite}
		if d := mcpBusiness.PrincipalCanReach(viaToken, anaID, ref); d.Allow {
			t.Errorf("MCP's reach lets Ana's token change work in Bo's project: %s", d.Reason)
		}
	})

	t.Run("an agent job a token queued keeps that in Postgres, for the worker", func(t *testing.T) {
		agentID := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Crier', $2)`, agentID, ana.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		agent := &agentModel.AiAgent{Id: agentID, Name: "Crier", CreatedBy: ana.UserPostgresInfo.Id}
		asked := func(how context.Context) map[string]any {
			t.Helper()
			post := uuid.NewString()
			if !aiAgentBusiness.EnqueueDurableAgentRun(how, agent, aiAgentBusiness.Surface{
				Kind: aiAgentBusiness.SurfaceChannelPost, ChannelID: announcements.String(), PostID: post}, "sum up the week", anaID) {
				t.Fatal("the job was not queued")
			}
			var raw string
			if err := env.PG.QueryRow(`SELECT surface::text FROM ai_agent_tasks WHERE agent_id = $1 AND source_id = $2`, agentID, post).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var surface map[string]any
			if err := json.Unmarshal([]byte(raw), &surface); err != nil {
				t.Fatalf("the job's surface %q: %v", raw, err)
			}
			return surface
		}
		if s := asked(viaToken); s["asked_through_token"] != true {
			t.Errorf("a job a token queued doesn't say so, so the worker runs it as its asker in the app: %v", s)
		}
		if s := asked(ctx); s["asked_through_token"] == true {
			t.Errorf("a job asked for in the app is held to a token's powers: %v", s)
		}
	})
}
