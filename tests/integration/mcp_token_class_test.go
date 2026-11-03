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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
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
	if err := mcpBusiness.PublishOps(); err != nil && !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("publish the API's operations as MCP tools: %v", err)
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
	// callAs sends a request with a token, as its holder's script would.
	callAs := func(plaintext, method, path string, body any) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	call := func(method, path string, body any) (int, map[string]any) {
		return callAs(anaToken.Plaintext, method, path, body)
	}
	// toolAs calls an MCP tool over /v1/mcp with a token, and answers its
	// text, or the refusal's message: "refused: " from the ladder, "failed: "
	// from the tool, "turned away: " from the /v1 door.
	toolAs := func(plaintext, name string, args map[string]any) (text string, refusal string) {
		code, out := callAs(plaintext, "POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		if code != http.StatusOK {
			return "", fmt.Sprintf("turned away: %d %v", code, out)
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
	tool := func(name string, args map[string]any) (text string, refusal string) {
		return toolAs(anaToken.Plaintext, name, args)
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

	t.Run("over the public API, the tables ops read and change rows as a member, and say who changed them", func(t *testing.T) {
		// tables.rows.query and tables.row.update (tables H1), declared once in
		// business/DataTable: what Ana's token reads and changes is what she
		// can as a member, a bad query is hers to fix (400, never 500), and
		// every write names her, her token and the agent it is bound to, in
		// the row ledger and in the audit log (main 12:4x, (a) and (b)).
		title, err := dataTableBusiness.CreateField(ctx, roadmap.Id, dataTableBusiness.FieldInput{Name: "Title", Type: "text"}, boActor)
		if err != nil {
			t.Fatalf("give the workspace's table a field: %v", err)
		}
		row := func(table, field uuid.UUID, text string) uuid.UUID {
			t.Helper()
			r, err := dataTableBusiness.CreateRow(ctx, table, dataTableBusiness.RowInput{Values: map[string]interface{}{field.String(): text}}, boActor)
			if err != nil {
				t.Fatalf("add the row %q: %v", text, err)
			}
			return r.Id
		}
		launch := row(roadmap.Id, title.Id, "Launch")
		row(roadmap.Id, title.Id, "Hiring")
		pay, err := dataTableBusiness.CreateField(ctx, salaries.Id, dataTableBusiness.FieldInput{Name: "Pay", Type: "text"}, boActor)
		if err != nil {
			t.Fatalf("give Bo's private table a field: %v", err)
		}
		secret := row(salaries.Id, pay.Id, "100")

		// Cy's token reads tables and changes none: a viewer's. The Clerk is
		// Ana's agent, and its token is bound to it.
		cyReader, err := apiTokenBusiness.CreateToken(ctx, "Cy's reader", []string{apiTokenBusiness.ScopeTablesRead}, 30, cy.UserPostgresInfo.Id, nil, false)
		if err != nil {
			t.Fatalf("make Cy's reading token: %v", err)
		}
		clerkID := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Clerk', $2)`, clerkID, ana.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		clerk, err := apiTokenBusiness.CreateToken(ctx, "The Clerk's key", []string{apiTokenBusiness.ScopeTablesRead, apiTokenBusiness.ScopeTablesWrite}, 30, ana.UserPostgresInfo.Id, &clerkID, true)
		if err != nil {
			t.Fatalf("make the Clerk's token: %v", err)
		}

		send := func(plaintext, method, path string, body any, headers ...string) (*httptest.ResponseRecorder, map[string]any) {
			t.Helper()
			b, _ := json.Marshal(body)
			req := httptest.NewRequest(method, path, bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+plaintext)
			req.Header.Set("Content-Type", "application/json")
			for i := 0; i+1 < len(headers); i += 2 {
				req.Header.Set(headers[i], headers[i+1])
			}
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec, out
		}
		query := func(table uuid.UUID) string { return "/v1/tables/" + table.String() + "/rows/query" }
		rowAt := func(table, r uuid.UUID) string { return "/v1/tables/" + table.String() + "/rows/" + r.String() }
		titled := func(op string, text string) map[string]any {
			return map[string]any{"op": "and", "filters": []any{map[string]any{
				"field": "field:" + title.Id.String(), "operator": op, "values": []any{map[string]any{"text": text}}}}}
		}
		titles := func(out map[string]any) (got []string) {
			items, _ := out["items"].([]any)
			for _, it := range items {
				m, _ := it.(map[string]any)
				raw, _ := m["values"].(string)
				var cells map[string]any
				_ = json.Unmarshal([]byte(raw), &cells)
				s, _ := cells[title.Id.String()].(string)
				got = append(got, s)
			}
			return got
		}

		// tables.rows.query reads what a member reads, filtered as a view is.
		w, out := send(anaToken.Plaintext, "POST", query(roadmap.Id), map[string]any{"filter": titled("contains", "Laun")})
		if got := titles(out); w.Code != http.StatusOK || len(got) != 1 || got[0] != "Launch" {
			t.Errorf("POST rows/query filtered to Laun… answered %d with %v, want 200 with Launch alone: %v", w.Code, got, out)
		}
		// The untitled row the first subtest added (when it ran) is left out:
		// where an empty title sorts is the query's rule, not this op's.
		w, out = send(cyReader.Plaintext, "POST", query(roadmap.Id), map[string]any{"sorts": []any{map[string]any{"field": "field:" + title.Id.String()}}})
		named := slices.DeleteFunc(titles(out), func(s string) bool { return s == "" })
		if w.Code != http.StatusOK || strings.Join(named, ",") != "Hiring,Launch" {
			t.Errorf("Cy's reader sorted by title: %d %v, want 200 Hiring,Launch: %v", w.Code, named, out)
		}
		// An admin's token opens no table a member can't, read or write.
		if w, out := send(anaToken.Plaintext, "POST", query(salaries.Id), map[string]any{}); w.Code != http.StatusNotFound {
			t.Errorf("POST rows/query on Bo's private table answered %d, want 404: an admin's power through a token: %v", w.Code, out)
		}
		if w, out := send(anaToken.Plaintext, "PATCH", rowAt(salaries.Id, secret), map[string]any{"values": map[string]any{pay.Id.String(): "1"}}); w.Code != http.StatusNotFound {
			t.Errorf("PATCH a row of Bo's private table answered %d, want 404: %v", w.Code, out)
		}

		// A bad query is the caller's to fix: 400 in the one error body.
		for name, body := range map[string]map[string]any{
			"a filter that isn't one":      {"filter": "everything"},
			"an operator that isn't one":   {"filter": titled("resembles", "Launch")},
			"a field the table hasn't got": {"filter": map[string]any{"op": "and", "filters": []any{map[string]any{"field": "field:" + uuid.NewString(), "operator": "contains", "values": []any{map[string]any{"text": "x"}}}}}},
			"a sort by a field it hasn't":  {"sorts": []any{map[string]any{"field": "field:" + uuid.NewString()}}},
		} {
			w, out := send(anaToken.Plaintext, "POST", query(roadmap.Id), body)
			e, _ := out["error"].(map[string]any)
			if msg, _ := e["message"].(string); w.Code != http.StatusBadRequest || e["code"] != "invalid_argument" || msg == "" {
				t.Errorf("%s answered %d %v, want 400 with the error body's invalid_argument and its words", name, w.Code, out)
			}
		}

		// tables.row.update changes the cells named, at the version read.
		w, out = send(anaToken.Plaintext, "PATCH", rowAt(roadmap.Id, launch), map[string]any{"values": map[string]any{title.Id.String(): "Launch day"}}, "If-Match", `W/"1"`)
		if w.Code != http.StatusOK || w.Header().Get("ETag") != `W/"2"` || !strings.Contains(fmt.Sprint(out["values"]), "Launch day") {
			t.Fatalf("PATCH the row at version 1 answered %d, ETag %q: %v; want 200, W/\"2\" and the new title", w.Code, w.Header().Get("ETag"), out)
		}
		// Its answer names the change set it stored (V-360): an undo names it
		// to take back this write, whatever lands after it.
		lastSet := func() string {
			t.Helper()
			var id uuid.UUID
			if err := env.PG.QueryRow(`SELECT id FROM data_table_change_sets WHERE table_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, roadmap.Id).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id.String()
		}
		if got, want := out["change_set"], lastSet(); got != want {
			t.Errorf("PATCH at version 1 answered change_set %v, want %s, the change set it stored", got, want)
		}
		if w, out := send(anaToken.Plaintext, "PATCH", rowAt(roadmap.Id, launch), map[string]any{"values": map[string]any{title.Id.String(): "Lunch"}}, "If-Match", `W/"1"`); w.Code != http.StatusPreconditionFailed {
			t.Errorf("PATCH at a version the row has moved on from answered %d, want 412: %v", w.Code, out)
		}
		// A viewer's token changes nothing.
		if w, out := send(cyReader.Plaintext, "PATCH", rowAt(roadmap.Id, launch), map[string]any{"values": map[string]any{title.Id.String(): "Cy was here"}}); w.Code != http.StatusForbidden {
			t.Errorf("a viewer's PATCH answered %d, want 403: %v", w.Code, out)
		}

		// Who changed it: the ledger's change set, and the audit log's record.
		changedBy := func() (actor uuid.UUID, agent *uuid.UUID, kind string) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT actor, agent, actor_kind FROM data_table_change_sets WHERE table_id = $1 ORDER BY at DESC, id DESC LIMIT 1`,
				roadmap.Id).Scan(&actor, &agent, &kind); err != nil {
				t.Fatal(err)
			}
			return actor, agent, kind
		}
		recorded := func() (actor uuid.UUID, kind, token, agent, table string) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT actor_id, actor_kind, COALESCE(metadata->>'token_id', ''), COALESCE(metadata->>'agent_id', ''),
				COALESCE(metadata->>'resource_id', '') FROM admin_audit_log WHERE action = 'api.tables.row.update'
				ORDER BY created_at DESC LIMIT 1`).Scan(&actor, &kind, &token, &agent, &table); err != nil {
				t.Fatalf("no audit record of the PATCH: %v", err)
			}
			return actor, kind, token, agent, table
		}
		if actor, agent, kind := changedBy(); actor != ana.UserPostgresInfo.Id || agent != nil || kind != "api" {
			t.Errorf("the ledger has Ana's PATCH by %s, agent %v, kind %q; want Ana, no agent, api", actor, agent, kind)
		}
		if actor, kind, token, agent, table := recorded(); actor != ana.UserPostgresInfo.Id || kind != "human" || token != anaToken.Token.Id.String() || agent != "" || table != roadmap.Id.String() {
			t.Errorf("the audit log has Ana's PATCH by %s (%s), token %q, agent %q, table %q; want Ana (human), her token, no agent, the Roadmap",
				actor, kind, token, agent, table)
		}
		w, out = send(clerk.Plaintext, "PATCH", rowAt(roadmap.Id, launch), map[string]any{"values": map[string]any{title.Id.String(): "Launch week"}})
		if w.Code != http.StatusOK {
			t.Fatalf("the Clerk's PATCH answered %d: %v", w.Code, out)
		}
		if got, want := out["change_set"], lastSet(); got != want {
			t.Errorf("the Clerk's PATCH, at no version, answered change_set %v, want %s, the change set it stored", got, want)
		}
		if actor, agent, kind := changedBy(); actor != ana.UserPostgresInfo.Id || agent == nil || *agent != clerkID || kind != "agent" {
			t.Errorf("the ledger has the Clerk's PATCH by %s, agent %v, kind %q; want Ana for the Clerk %s, agent", actor, agent, kind, clerkID)
		}
		if actor, kind, token, agent, _ := recorded(); actor != ana.UserPostgresInfo.Id || kind != "agent" || token != clerk.Token.Id.String() || agent != clerkID.String() {
			t.Errorf("the audit log has the Clerk's PATCH by %s (%s), token %q, agent %q; want Ana (agent), the Clerk's token, the Clerk", actor, kind, token, agent)
		}

		// A token that has written its minute's rows waits: 429, and when.
		// The Clerk's PATCH above spent a row of this minute, so a whole
		// budget more passes it: refused, and spent all the same (the counter
		// rises before it is compared).
		spent := context.WithValue(ctx, helpers.ApiTokenIDContextKey, clerk.Token.Id.String())
		if err := v1api.ChargeRows(spent, clerk.Token.Id.String(), false, v1api.RowWritesPerMinute); err != nil {
			var refused *v1api.Error
			if !errors.As(err, &refused) || refused.Code != "row_budget" {
				t.Fatalf("spend the Clerk's minute: %v", err)
			}
		}
		w, out = send(clerk.Plaintext, "PATCH", rowAt(roadmap.Id, launch), map[string]any{"values": map[string]any{title.Id.String(): "Launch month"}})
		if e, _ := out["error"].(map[string]any); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || e["code"] != "row_budget" {
			t.Errorf("a PATCH past the token's row budget answered %d, Retry-After %q: %v; want 429 row_budget with Retry-After", w.Code, w.Header().Get("Retry-After"), out)
		}

		// Over MCP, the same two ops published as tools (part C): listed for a
		// token whose scopes hold them, with their hints and Input schema, and
		// answering, refusing and recording as REST does (condition c).
		listed := func(plaintext string) map[string]map[string]any {
			t.Helper()
			w, out := send(plaintext, "POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			res, _ := out["result"].(map[string]any)
			list, _ := res["tools"].([]any)
			got := map[string]map[string]any{}
			for _, x := range list {
				m, _ := x.(map[string]any)
				name, _ := m["name"].(string)
				got[name] = m
			}
			if w.Code != http.StatusOK || len(got) == 0 {
				t.Fatalf("tools/list answered %d with no tools: %v", w.Code, out)
			}
			return got
		}
		hint := func(tool map[string]any, name string) bool {
			a, _ := tool["annotations"].(map[string]any)
			b, _ := a[name].(bool)
			return b
		}
		all := listed(anaToken.Plaintext)
		q, u := all["tables_rows_query"], all["tables_row_update"]
		if q == nil || u == nil {
			t.Fatalf("tools/list for Ana's token leaves out the tables ops: query %v, update %v", q != nil, u != nil)
		}
		if !hint(q, "readOnlyHint") || hint(u, "readOnlyHint") || !hint(u, "idempotentHint") || hint(u, "destructiveHint") {
			t.Errorf("the ops' hints aren't their declared behaviour: query %v, update %v", q["annotations"], u["annotations"])
		}
		if schema, _ := u["inputSchema"].(map[string]any); !strings.Contains(fmt.Sprint(schema["required"]), "values") {
			t.Errorf("tables_row_update's inputSchema isn't the op's Input: %v", u["inputSchema"])
		}
		if read := listed(cyReader.Plaintext); read["tables_rows_query"] == nil || read["tables_row_update"] != nil {
			t.Errorf("tools/list for Cy's reading token: query listed %v, update listed %v; want the read alone", read["tables_rows_query"] != nil, read["tables_row_update"] != nil)
		}

		if text, refusal := tool("tables_rows_query", map[string]any{"id": roadmap.Id.String(), "filter": titled("contains", "Hir")}); refusal != "" ||
			!strings.Contains(text, "Hiring") || strings.Contains(text, "Launch") {
			t.Errorf("tables_rows_query filtered to Hir… answered %q %q, want Hiring alone", text, refusal)
		}
		if _, refusal := tool("tables_rows_query", map[string]any{"id": roadmap.Id.String(), "filter": titled("resembles", "x")}); !strings.HasPrefix(refusal, "failed: ") ||
			!strings.Contains(refusal, "resembles") {
			t.Errorf("a bad filter over MCP answered %q, want the tool's error in the query's words", refusal)
		}
		if _, refusal := tool("tables_rows_query", map[string]any{"id": salaries.Id.String()}); !strings.HasPrefix(refusal, "refused: ") {
			t.Errorf("tables_rows_query on Bo's private table answered %q, want it refused", refusal)
		}
		// Bo's workspace table takes Ana's row change over MCP as over REST: the
		// rows' rule, not CanManage (she didn't make the table).
		text, refusal := tool("tables_row_update", map[string]any{"id": roadmap.Id.String(), "row": launch.String(),
			"values": map[string]any{title.Id.String(): "Launch over MCP"}})
		if refusal != "" || !strings.Contains(text, "Launch over MCP") {
			t.Errorf("tables_row_update on the workspace's table answered %q %q, want the row changed as REST changes it", text, refusal)
		}
		if actor, agent, kind := changedBy(); actor != ana.UserPostgresInfo.Id || agent != nil || kind != "api" {
			t.Errorf("the ledger has Ana's MCP change by %s, agent %v, kind %q; want Ana, no agent, api", actor, agent, kind)
		}
		if actor, kind, token, _, _ := recorded(); actor != ana.UserPostgresInfo.Id || kind != "human" || token != anaToken.Token.Id.String() {
			t.Errorf("the audit log has Ana's MCP change by %s (%s), token %q; want Ana (human), her token", actor, kind, token)
		}
		if _, refusal := tool("tables_row_update", map[string]any{"id": roadmap.Id.String(), "row": launch.String(),
			"values": map[string]any{title.Id.String(): "Lunch"}, "version": 1}); !strings.HasPrefix(refusal, "failed: ") {
			t.Errorf("a stale version over MCP answered %q, want the tool's version error", refusal)
		}
		if _, refusal := tool("tables_row_update", map[string]any{"id": salaries.Id.String(), "row": secret.String(),
			"values": map[string]any{pay.Id.String(): "1"}}); !strings.HasPrefix(refusal, "refused: ") {
			t.Errorf("tables_row_update on Bo's private table answered %q, want it refused", refusal)
		}
	})

	t.Run("over the public API, a member reads rows by id, writes a batch as one change, and deletes to the trash and back", func(t *testing.T) {
		// tables H2a: the rows ops beside H1's, declared once in
		// business/DataTable and published on REST and MCP alike. A row is
		// read by its id, its version the ETag a PATCH's If-Match names; a
		// batch makes, changes and deletes rows in one change set, or writes
		// nothing and says which rows it refused; a delete is soft (main
		// 11:2x, (a)): the row waits in the table's trash, which brings it
		// back with its cells.
		stage, err := dataTableBusiness.CreateField(ctx, roadmap.Id, dataTableBusiness.FieldInput{Name: "Stage", Type: "text"}, boActor)
		if err != nil {
			t.Fatalf("give the Roadmap a Stage field: %v", err)
		}
		points, err := dataTableBusiness.CreateField(ctx, roadmap.Id, dataTableBusiness.FieldInput{Name: "Points", Type: "number"}, boActor)
		if err != nil {
			t.Fatalf("give the Roadmap a Points field: %v", err)
		}
		reader, err := apiTokenBusiness.CreateToken(ctx, "Cy's rows reader", []string{apiTokenBusiness.ScopeTablesRead}, 30, cy.UserPostgresInfo.Id, nil, false)
		if err != nil {
			t.Fatalf("make Cy's reading token: %v", err)
		}
		send := func(plaintext, method, path string, body any, headers ...string) (*httptest.ResponseRecorder, map[string]any) {
			t.Helper()
			var b []byte
			if body != nil {
				b, _ = json.Marshal(body)
			}
			req := httptest.NewRequest(method, path, bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+plaintext)
			req.Header.Set("Content-Type", "application/json")
			for i := 0; i+1 < len(headers); i += 2 {
				req.Header.Set(headers[i], headers[i+1])
			}
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec, out
		}
		listOf := func(v any) []map[string]any {
			list, _ := v.([]any)
			out := make([]map[string]any, 0, len(list))
			for _, x := range list {
				m, _ := x.(map[string]any)
				out = append(out, m)
			}
			return out
		}
		cells := func(row map[string]any) map[string]any {
			raw, _ := row["values"].(string)
			var c map[string]any
			_ = json.Unmarshal([]byte(raw), &c)
			return c
		}
		ana, s := anaToken.Plaintext, stage.Id.String()
		base := "/v1/tables/" + roadmap.Id.String()
		batches := 0
		batch := func(body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
			t.Helper()
			batches++
			return send(ana, "POST", base+"/rows/batch", body, "Idempotency-Key", fmt.Sprintf("h2a-batch-%d-%s", batches, roadmap.Id))
		}

		// One batch makes two rows; the next makes one, changes one at the
		// version it was read and deletes one, in one change set.
		w, out := batch(map[string]any{"create": []any{
			map[string]any{"values": map[string]any{s: "Idea"}}, map[string]any{"values": map[string]any{s: "Draft"}}}})
		made := listOf(out["created"])
		if w.Code != http.StatusCreated || len(made) != 2 || out["change_set"] == nil {
			t.Fatalf("a batch making two rows answered %d: %v; want 201, both rows and their change set", w.Code, out)
		}
		idea, _ := made[0]["id"].(string)
		draft, _ := made[1]["id"].(string)
		w, out = batch(map[string]any{
			"create": []any{map[string]any{"values": map[string]any{s: "Spare"}}},
			"update": []any{map[string]any{"row": idea, "values": map[string]any{s: "Plan"}, "version": 1}},
			"delete": []any{draft}})
		changed, spares := listOf(out["updated"]), listOf(out["created"])
		if w.Code != http.StatusCreated || len(changed) != 1 || cells(changed[0])[s] != "Plan" || len(spares) != 1 ||
			fmt.Sprint(out["deleted"]) != "["+draft+"]" {
			t.Fatalf("a batch making, changing and deleting rows answered %d: %v", w.Code, out)
		}
		together, _ := out["change_set"].(string)
		spare, _ := spares[0]["id"].(string)

		// A batch with one bad cell writes nothing and names the row; one
		// past a thousand rows in all is refused whole.
		w, out = batch(map[string]any{"update": []any{
			map[string]any{"row": idea, "values": map[string]any{s: "Ship"}},
			map[string]any{"row": spare, "values": map[string]any{points.Id.String(): "lots"}}}})
		refusal, _ := out["error"].(map[string]any)
		details := listOf(refusal["details"])
		if w.Code != http.StatusBadRequest || refusal["code"] != "invalid_argument" || len(details) != 1 || details[0]["row"] != spare {
			t.Errorf("a batch with a word in a number field answered %d: %v; want 400 invalid_argument naming that row alone", w.Code, out)
		}
		creates, deletes := make([]any, 600), make([]any, 401)
		for i := range creates {
			creates[i] = map[string]any{"values": map[string]any{s: "More"}}
		}
		for i := range deletes {
			deletes[i] = uuid.NewString()
		}
		if w, out := batch(map[string]any{"create": creates, "delete": deletes}); w.Code != http.StatusBadRequest {
			t.Errorf("a batch of 1,001 rows answered %d: %v; want 400", w.Code, out)
		}

		// A row by its id, at the version the bad batch left alone; a
		// deleted one isn't there; rows by their ids leave a deleted one out.
		w, out = send(ana, "GET", base+"/rows/"+idea, nil)
		if w.Code != http.StatusOK || cells(out)[s] != "Plan" || w.Header().Get("ETag") != `W/"2"` {
			t.Errorf("GET the changed row answered %d, ETag %q: %v; want 200, W/\"2\" and Plan", w.Code, w.Header().Get("ETag"), out)
		}
		if w, out := send(ana, "GET", base+"/rows/"+draft, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET a deleted row answered %d: %v; want 404", w.Code, out)
		}
		w, out = send(ana, "POST", base+"/rows/get", map[string]any{"ids": []any{spare, draft, idea}})
		got := listOf(out["items"])
		if w.Code != http.StatusOK || len(got) != 2 || got[0]["id"] != spare || got[1]["id"] != idea {
			t.Errorf("POST rows/get for the spare, the deleted draft and the idea answered %d: %v; want the spare, then the idea", w.Code, out)
		}

		// A delete goes to the trash, and the trash brings the row back.
		if w, out := send(ana, "DELETE", base+"/rows/"+spare, nil, "Idempotency-Key", "h2a-delete-"+spare); w.Code != http.StatusOK {
			t.Fatalf("DELETE the spare row answered %d: %v", w.Code, out)
		}
		if w, out := send(ana, "GET", base+"/rows/"+spare, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET a row just deleted answered %d: %v; want 404", w.Code, out)
		}
		w, out = send(ana, "GET", base+"/trash", nil)
		var trashed []string
		for _, r := range listOf(out["items"]) {
			id, _ := r["id"].(string)
			trashed = append(trashed, id)
		}
		if w.Code != http.StatusOK || !strings.Contains(fmt.Sprint(trashed), spare) || !strings.Contains(fmt.Sprint(trashed), draft) {
			t.Errorf("GET the trash answered %d with %v: %v; want the spare and the draft in it", w.Code, trashed, out)
		}
		w, out = send(ana, "POST", base+"/trash/restore", map[string]any{"rows": []any{spare}}, "Idempotency-Key", "h2a-restore-"+spare)
		if w.Code != http.StatusCreated || fmt.Sprint(out["rows"]) != "["+spare+"]" {
			t.Fatalf("POST trash/restore for the spare row answered %d: %v", w.Code, out)
		}
		if w, out := send(ana, "GET", base+"/rows/"+spare, nil); w.Code != http.StatusOK || cells(out)[s] != "Spare" {
			t.Errorf("GET the row brought back answered %d: %v; want 200 with its cells", w.Code, out)
		}

		// A row's history, newest first: the batch's one change set, by the API.
		w, out = send(ana, "GET", base+"/rows/"+idea+"/history", nil)
		items := listOf(out["items"])
		if w.Code != http.StatusOK || len(items) < 2 || items[0]["change_set"] != together || items[0]["actor_kind"] != "api" {
			t.Errorf("GET the idea's history answered %d: %v; want the batch's change set first, made by the API", w.Code, out)
		}

		// A reading token deletes nothing; Bo's private table is one that
		// isn't there, for an admin's token too.
		if w, out := send(reader.Plaintext, "DELETE", base+"/rows/"+idea, nil); w.Code != http.StatusForbidden {
			t.Errorf("a reading token's DELETE answered %d: %v; want 403", w.Code, out)
		}
		private := "/v1/tables/" + salaries.Id.String()
		if w, out := send(ana, "POST", private+"/rows/get", map[string]any{"ids": []any{uuid.NewString()}}); w.Code != http.StatusNotFound {
			t.Errorf("rows/get on Bo's private table answered %d: %v; want 404", w.Code, out)
		}
		if w, out := send(ana, "GET", private+"/trash", nil); w.Code != http.StatusNotFound {
			t.Errorf("GET the trash of Bo's private table answered %d: %v; want 404", w.Code, out)
		}

		// Over MCP, the same ops are tools, with their declared hints.
		w, out = send(ana, "POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		res, _ := out["result"].(map[string]any)
		tools := map[string]map[string]any{}
		for _, x := range listOf(res["tools"]) {
			name, _ := x["name"].(string)
			tools[name] = x
		}
		hint := func(name, h string) bool {
			a, _ := tools[name]["annotations"].(map[string]any)
			b, _ := a[h].(bool)
			return b
		}
		for _, name := range []string{"tables_row_get", "tables_rows_get", "tables_rows_batch", "tables_row_delete",
			"tables_trash_list", "tables_trash_restore", "tables_row_history"} {
			if tools[name] == nil {
				t.Errorf("tools/list (%d) for Ana's token leaves out %s", w.Code, name)
			}
		}
		if !hint("tables_rows_batch", "destructiveHint") || hint("tables_rows_batch", "idempotentHint") ||
			!hint("tables_row_delete", "destructiveHint") || !hint("tables_row_delete", "idempotentHint") ||
			!hint("tables_rows_get", "readOnlyHint") || !hint("tables_row_history", "readOnlyHint") || hint("tables_trash_restore", "readOnlyHint") {
			t.Errorf("the rows ops' hints aren't their declared behaviour: batch %v, delete %v", tools["tables_rows_batch"]["annotations"],
				tools["tables_row_delete"]["annotations"])
		}

		// A batch refused over MCP names its rows as REST's does: in the
		// structured error's details, and in the text, which is all many
		// clients hand their model, so "fix the rows the details name" can be
		// done. The idea is at version 2, so a change at version 1 is stale.
		w, out = send(ana, "POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "tables_rows_batch", "arguments": map[string]any{"id": roadmap.Id.String(),
				"update": []any{map[string]any{"row": idea, "values": map[string]any{s: "Ship"}, "version": 1}}}}})
		res, _ = out["result"].(map[string]any)
		structured, _ := res["structuredContent"].(map[string]any)
		refused, _ := structured["error"].(map[string]any)
		named := listOf(refused["details"])
		var text string
		for _, part := range listOf(res["content"]) {
			words, _ := part["text"].(string)
			text += words
		}
		if isErr, _ := res["isError"].(bool); w.Code != http.StatusOK || !isErr || refused["code"] != "invalid_argument" ||
			len(named) != 1 || named[0]["row"] != idea || !strings.Contains(text, idea) {
			t.Errorf("a stale tables_rows_batch over MCP answered %d: %v; want an error whose details and text name the idea's row", w.Code, out)
		}
		if w, out := send(ana, "GET", base+"/rows/"+idea, nil); w.Code != http.StatusOK || cells(out)[s] != "Plan" {
			t.Errorf("after the refused batch over MCP, GET the idea answered %d: %v; want it unchanged (Plan)", w.Code, out)
		}

		// Main's ruling A (21:1x, 2 Nov): the batch and the delete are
		// Recoverable, their undo published beside them, so over MCP an
		// agent's token runs them with no approval card. Each audit row names
		// the token, the agent, the member it acts for and the change, whose
		// ledger rows are every row the call touched; the trash brings a row
		// back over MCP; a paused agent is turned away before a batch starts;
		// and the token's budget counts rows, not calls.
		anaUser, porterID := anaToken.Token.CreatedBy, uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, enabled_tools)
			VALUES ($1, 'Porter', $2, 'mention', '["tables_rows_batch", "tables_row_delete", "tables_trash_restore"]'::jsonb)`, porterID, anaUser); err != nil {
			t.Fatal(err)
		}
		porter, err := apiTokenBusiness.CreateToken(ctx, "The Porter's key", []string{apiTokenBusiness.ScopeTablesRead, apiTokenBusiness.ScopeTablesWrite},
			30, anaUser, &porterID, false)
		if err != nil {
			t.Fatalf("make the Porter's token: %v", err)
		}
		porterCall := func(name string, args map[string]any) (*httptest.ResponseRecorder, map[string]any) {
			t.Helper()
			return send(porter.Plaintext, "POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": name, "arguments": args}})
		}
		porterTool := func(name string, args map[string]any) map[string]any {
			t.Helper()
			w, out := porterCall(name, args)
			res, _ := out["result"].(map[string]any)
			var answer map[string]any
			for _, part := range listOf(res["content"]) {
				words, _ := part["text"].(string)
				_ = json.Unmarshal([]byte(words), &answer)
			}
			if isErr, _ := res["isError"].(bool); w.Code != http.StatusOK || isErr || answer == nil {
				t.Fatalf("%s over MCP by the Porter answered %d: %v; want it run, with no approval card", name, w.Code, out)
			}
			return answer
		}
		// audited is the latest audit row of op: the member, how, the token,
		// the agent and the change; and every row that change touched.
		audited := func(op string) (actor uuid.UUID, kind, token, agent, change string, rows map[string]bool) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT actor_id, actor_kind, COALESCE(metadata->>'token_id', ''), COALESCE(metadata->>'agent_id', ''),
				COALESCE(metadata->>'change_id', '') FROM admin_audit_log WHERE action = $1 ORDER BY created_at DESC LIMIT 1`, "api."+op).
				Scan(&actor, &kind, &token, &agent, &change); err != nil {
				t.Fatalf("no audit record of %s: %v", op, err)
			}
			found, err := env.PG.Query(`SELECT DISTINCT row_id::text FROM data_table_changes WHERE change_set::text = $1`, change)
			if err != nil {
				t.Fatal(err)
			}
			defer found.Close()
			rows = map[string]bool{}
			for found.Next() {
				var id string
				if err := found.Scan(&id); err != nil {
					t.Fatal(err)
				}
				rows[id] = true
			}
			return actor, kind, token, agent, change, rows
		}

		moved := porterTool("tables_rows_batch", map[string]any{"id": roadmap.Id.String(),
			"create": []any{map[string]any{"values": map[string]any{s: "Crate"}}, map[string]any{"values": map[string]any{s: "Pallet"}}},
			"delete": []any{spare}})
		var crate, pallet string
		if news := listOf(moved["created"]); len(news) == 2 {
			crate, _ = news[0]["id"].(string)
			pallet, _ = news[1]["id"].(string)
		}
		actor, kind, token, agent, change, rows := audited("tables.rows.batch")
		if actor != anaUser || kind != "agent" || token != porter.Token.Id.String() || agent != porterID.String() || change == "" ||
			change != moved["change_set"] || len(rows) != 3 || !rows[crate] || !rows[pallet] || !rows[spare] {
			t.Errorf("the Porter's batch over MCP is audited by %s (%s), token %q, agent %q, change %q (it answered %v) touching %v; want Ana (agent), "+
				"the Porter's token, the Porter, the batch's change, and the crate, the pallet and the spare", actor, kind, token, agent, change, moved["change_set"], rows)
		}

		porterTool("tables_row_delete", map[string]any{"id": roadmap.Id.String(), "row": crate})
		if actor, kind, _, agent, change, rows := audited("tables.row.delete"); actor != anaUser || kind != "agent" || agent != porterID.String() ||
			change == "" || len(rows) != 1 || !rows[crate] {
			t.Errorf("the Porter's delete over MCP is audited by %s (%s), agent %q, change %q touching %v; want Ana (agent), the Porter, and the crate alone",
				actor, kind, agent, change, rows)
		}
		if w, out := send(ana, "GET", base+"/rows/"+crate, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET the crate the Porter deleted answered %d: %v; want 404, as it's in the trash", w.Code, out)
		}
		porterTool("tables_trash_restore", map[string]any{"id": roadmap.Id.String(), "rows": []any{crate}})
		if w, out := send(ana, "GET", base+"/rows/"+crate, nil); w.Code != http.StatusOK || cells(out)[s] != "Crate" {
			t.Errorf("GET the crate the Porter brought back over MCP answered %d: %v; want 200 with its cells", w.Code, out)
		}

		// Paused, the Porter is turned away before a batch starts: no change.
		changeSets := func() (n int) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT count(*) FROM data_table_change_sets WHERE agent = $1`, porterID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := changeSets()
		if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, porterID); err != nil {
			t.Fatal(err)
		}
		if w, out := porterCall("tables_rows_batch", map[string]any{"id": roadmap.Id.String(),
			"create": []any{map[string]any{"values": map[string]any{s: "Late"}}}}); w.Code == http.StatusOK || changeSets() != before {
			t.Errorf("the paused Porter's batch answered %d: %v, and its change sets went from %d to %d; want it turned away with no change",
				w.Code, out, before, changeSets())
		}

		// The budget counts rows: the batch spent 3 (2 made, 1 deleted), the
		// delete 1 and the restore 1, and the paused batch none, so the
		// Porter's minute (from its first write) has its budget less 5 left.
		spend := context.WithValue(ctx, helpers.ApiTokenIDContextKey, porter.Token.Id.String())
		if err := v1api.ChargeRows(spend, porter.Token.Id.String(), false, v1api.RowWritesPerMinute-5); err != nil {
			t.Errorf("the Porter's minute had less than its budget less 5 rows left: %v", err)
		}
		var past *v1api.Error
		if err := v1api.ChargeRows(spend, porter.Token.Id.String(), false, 1); !errors.As(err, &past) || past.Code != "row_budget" {
			t.Errorf("a row past the Porter's budget, less the 5 its calls spent, answered %v; want row_budget", err)
		}
	})

	// V-331 (main 13:2x, 2 Nov): the bridged row tools ask the rule the app and
	// REST ask of a table's rows (business/TableAccess's Edit), not CanManage.
	// Cy is a member and no manager of Bo's Roadmap: her token makes, changes
	// and links its rows over MCP as she does in the app; a table she can't
	// open is refused in REST's words, as one that isn't there is; and her
	// agent, paused, is turned away.
	t.Run("over MCP, a member who can edit a table makes, changes and links its rows, and no one else does", func(t *testing.T) {
		note, err := dataTableBusiness.CreateField(ctx, roadmap.Id, dataTableBusiness.FieldInput{Name: "Note", Type: "text"}, boActor)
		if err != nil {
			t.Fatalf("give the Roadmap a note: %v", err)
		}
		vendors, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Vendors", Visibility: "workspace"}, boActor)
		if err != nil {
			t.Fatalf("make Bo's vendors: %v", err)
		}
		name, err := dataTableBusiness.CreateField(ctx, vendors.Id, dataTableBusiness.FieldInput{Name: "Name", Type: "text"}, boActor)
		if err != nil {
			t.Fatalf("give the vendors a name: %v", err)
		}
		acme, err := dataTableBusiness.CreateRow(ctx, vendors.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name.Id.String(): "Acme"}}, boActor)
		if err != nil {
			t.Fatalf("add Acme: %v", err)
		}
		vendor, err := dataTableBusiness.CreateField(ctx, roadmap.Id, dataTableBusiness.FieldInput{Name: "Vendor", Type: "relation",
			Config: map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()}}, boActor)
		if err != nil {
			t.Fatalf("link the Roadmap to the vendors: %v", err)
		}
		cell := func(row, field uuid.UUID) string {
			t.Helper()
			var v string
			if err := env.PG.QueryRow(`SELECT COALESCE("values"->>$2, '') FROM data_table_rows WHERE id = $1`, row, field.String()).Scan(&v); err != nil {
				t.Fatalf("read row %s: %v", row, err)
			}
			return v
		}
		lastBy := func() uuid.UUID {
			t.Helper()
			var actor uuid.UUID
			if err := env.PG.QueryRow(`SELECT actor FROM data_table_change_sets WHERE table_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, roadmap.Id).Scan(&actor); err != nil {
				t.Fatalf("read the Roadmap's ledger: %v", err)
			}
			return actor
		}
		rows := func(table uuid.UUID) (n int) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT count(*) FROM data_table_rows WHERE table_id = $1 AND deleted_at IS NULL`, table).Scan(&n); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			return n
		}
		cyID := cy.UserPostgresInfo.Id

		text, refusal := toolAs(cyToken.Plaintext, "create_table_row", map[string]any{"table_uuid": roadmap.Id.String(),
			"values": map[string]any{note.Id.String(): "Vendor review"}})
		made, perr := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(text, "Created row "), "."))
		if refusal != "" || perr != nil {
			t.Fatalf("create_table_row by Cy on the Roadmap answered %q %q, want the row made", text, refusal)
		}
		if got, by := cell(made, note.Id), lastBy(); got != "Vendor review" || by != cyID {
			t.Errorf("Cy's new row reads %q, made by %s; want \"Vendor review\" by Cy %s", got, by, cyID)
		}
		if text, refusal := toolAs(cyToken.Plaintext, "update_table_row", map[string]any{"table_uuid": roadmap.Id.String(),
			"row_uuid": made.String(), "values": map[string]any{note.Id.String(): "Vendor review, signed"}}); refusal != "" ||
			!strings.HasPrefix(text, "Updated row") {
			t.Errorf("update_table_row by Cy answered %q %q, want the row changed", text, refusal)
		}
		if got, by := cell(made, note.Id), lastBy(); got != "Vendor review, signed" || by != cyID {
			t.Errorf("Cy's changed row reads %q, changed by %s; want \"Vendor review, signed\" by Cy", got, by)
		}
		if text, refusal := toolAs(cyToken.Plaintext, "link_table_rows", map[string]any{"table_uuid": roadmap.Id.String(),
			"row_uuid": made.String(), "field_uuid": vendor.Id.String(), "add": []string{acme.Id.String()}}); refusal != "" ||
			!strings.Contains(text, "1 links made") {
			t.Errorf("link_table_rows by Cy answered %q %q, want Acme linked", text, refusal)
		}
		var linked int
		if err := env.PG.QueryRow(`SELECT count(*) FROM data_table_links WHERE field_id = $1 AND from_row = $2 AND to_row = $3`,
			vendor.Id, made, acme.Id).Scan(&linked); err != nil || linked != 1 {
			t.Errorf("Cy's row has %d links to Acme by the Vendor field (%v), want 1", linked, err)
		}

		// A table Cy can't open: REST's answer, word for word, from every row
		// tool and the published op alike, and the same for one that isn't
		// there, so no answer tells which tables exist. Nothing is written.
		code, out := callAs(cyToken.Plaintext, "PATCH", "/v1/tables/"+salaries.Id.String()+"/rows/"+uuid.NewString(),
			map[string]any{"values": map[string]any{}})
		e, _ := out["error"].(map[string]any)
		rest, _ := e["message"].(string)
		if code != http.StatusNotFound || rest == "" {
			t.Fatalf("REST PATCH on Bo's private table by Cy answered %d %v, want 404 with a message", code, out)
		}
		before := rows(salaries.Id)
		for _, table := range []struct{ what, id string }{{"Bo's private table", salaries.Id.String()}, {"a table that isn't there", uuid.NewString()}} {
			for _, tl := range []string{"create_table_row", "update_table_row", "link_table_rows"} {
				_, refusal := toolAs(cyToken.Plaintext, tl, map[string]any{"table_uuid": table.id, "row_uuid": uuid.NewString(),
					"field_uuid": vendor.Id.String(), "values": map[string]any{note.Id.String(): "x"}, "add": []string{acme.Id.String()}})
				if refusal != "refused: "+rest {
					t.Errorf("%s by Cy on %s answered %q, want REST's answer %q", tl, table.what, refusal, rest)
				}
			}
			if _, refusal := toolAs(cyToken.Plaintext, "tables_row_update", map[string]any{"id": table.id, "row": uuid.NewString(),
				"values": map[string]any{}}); refusal != "refused: "+rest {
				t.Errorf("tables_row_update by Cy on %s answered %q, want REST's answer %q", table.what, refusal, rest)
			}
		}
		if after := rows(salaries.Id); after != before {
			t.Errorf("Bo's private table went from %d rows to %d under Cy's refused calls", before, after)
		}

		// Cy's agent: live, its token makes rows as she does; paused, it is
		// turned away at the door, and the ladder refuses it too.
		runnerID := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, enabled_tools)
			VALUES ($1, 'Runner', $2, 'mention', '["create_table_row"]'::jsonb)`, runnerID, cyID); err != nil {
			t.Fatal(err)
		}
		runner, err := apiTokenBusiness.CreateToken(ctx, "The Runner's key", []string{apiTokenBusiness.ScopeTablesRead, apiTokenBusiness.ScopeTablesWrite}, 30, cyID, &runnerID, false)
		if err != nil {
			t.Fatalf("make the Runner's token: %v", err)
		}
		args := func(text string) map[string]any {
			return map[string]any{"table_uuid": roadmap.Id.String(), "values": map[string]any{note.Id.String(): text}}
		}
		if text, refusal := toolAs(runner.Plaintext, "create_table_row", args("By the Runner")); refusal != "" || !strings.HasPrefix(text, "Created row") {
			t.Errorf("the live Runner's create_table_row answered %q %q, want a row made", text, refusal)
		}
		if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, runnerID); err != nil {
			t.Fatal(err)
		}
		before = rows(roadmap.Id)
		if text, refusal := toolAs(runner.Plaintext, "create_table_row", args("By the paused Runner")); text != "" ||
			!strings.HasPrefix(refusal, "turned away: ") || !strings.Contains(refusal, "deactivated") {
			t.Errorf("the paused Runner's create_table_row answered %q %q, want it turned away as deactivated", text, refusal)
		}
		if d := mcpBusiness.AuthorizeToolCall(ctx, mcpBusiness.ToolCallRequest{TokenPlaintext: runner.Plaintext, ToolName: "create_table_row",
			Args: args("By the paused Runner, past the door")}); d.Allow {
			t.Errorf("the ladder allowed the paused Runner's create_table_row (%s)", d.Reason)
		}
		if after := rows(roadmap.Id); after != before {
			t.Errorf("the Roadmap went from %d rows to %d under the paused Runner", before, after)
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
