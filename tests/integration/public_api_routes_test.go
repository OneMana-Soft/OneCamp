//go:build integration
// +build integration

package integration_test

// Every public API route works with a valid token holding its scope, in
// whichever edition this is built as, against real Postgres, Dgraph and Redis.
//
// Posting a message, searching and touching a task went through the AI
// assistant's tool executors, which the AI-free edition stubs: there each
// answered 501 "AI features are not supported on this build", while tokens
// with those scopes could be made, so "post to #deploys from CI" failed on
// every AI-free install (SA-BUG-11). The test walks the router's /v1 routes,
// so a route added without a case here fails it too.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEveryPublicAPIRouteAnswers -v

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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEveryPublicAPIRouteAnswers(t *testing.T) {
	t.Setenv("JWT_SECRET", "public-api-routes-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	// Search answers with nothing found; writes are accepted.
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
	owner := person("ci@example.test", "CI Owner")
	if err := userBusiness.CreateAdminUser(ctx, "ci@example.test"); err != nil {
		t.Fatal(err)
	}
	owner.UserPostgresInfo.IsAdmin = true
	jonas := person("jonas@example.test", "Jonas")

	err, deploys := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "deploys"}, &owner)
	if err != nil {
		t.Fatalf("make #deploys: %v", err)
	}
	project := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Website', $2)`, project, owner.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": owner.UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Website",
			"project_deleted_at": "0001-01-01T00:00:00Z",
			"project_members":    []map[string]any{{"uid": owner.UserDgraphInfo.Uid}},
			"project_admins":     []map[string]any{{"uid": owner.UserDgraphInfo.Uid}},
			"project_team":       map[string]any{"dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Web"}},
	})
	table, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Releases", Visibility: "workspace"},
		dataTableBusiness.Actor{UserID: owner.UserPostgresInfo.Id, IsAdmin: true})
	if err != nil {
		t.Fatalf("make a table: %v", err)
	}
	made, err := apiTokenBusiness.CreateToken(ctx, "ci", apiTokenBusiness.AllScopes, 30, owner.UserPostgresInfo.Id, nil, true)
	if err != nil {
		t.Fatalf("make a token: %v", err)
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
		req.Header.Set("Authorization", "Bearer "+made.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// One case per route, in an order where each has what it needs.
	var taskID string
	type step struct {
		route string // "METHOD /pattern" as the router has it
		run   func() (int, map[string]any)
	}
	steps := []step{
		{"GET /v1/me", func() (int, map[string]any) { return call("GET", "/v1/me", nil) }},
		{"GET /v1/unread", func() (int, map[string]any) { return call("GET", "/v1/unread", nil) }},
		{"GET /v1/attention", func() (int, map[string]any) { return call("GET", "/v1/attention", nil) }},
		{"GET /v1/tables", func() (int, map[string]any) { return call("GET", "/v1/tables", nil) }},
		{"GET /v1/tables/{id}", func() (int, map[string]any) { return call("GET", "/v1/tables/"+table.Id.String(), nil) }},
		{"POST /v1/tables/{id}/aggregate", func() (int, map[string]any) {
			return call("POST", "/v1/tables/"+table.Id.String()+"/aggregate", map[string]any{"aggregate": "count"})
		}},
		{"POST /v1/tables/{id}/rows", func() (int, map[string]any) {
			return call("POST", "/v1/tables/"+table.Id.String()+"/rows", map[string]any{"values": map[string]any{}})
		}},
		{"POST /v1/tasks", func() (int, map[string]any) {
			code, out := call("POST", "/v1/tasks", map[string]any{"task_name": "Ship the release notes", "project_uuid": project.String(), "priority": "high"})
			if d, ok := out["data"].(map[string]any); ok {
				taskID, _ = d["task_uuid"].(string)
			}
			return code, out
		}},
		{"POST /v1/tasks/{id}/status", func() (int, map[string]any) {
			return call("POST", "/v1/tasks/"+taskID+"/status", map[string]any{"status": "inProgress"})
		}},
		{"GET /v1/tasks", func() (int, map[string]any) { return call("GET", "/v1/tasks?search=release", nil) }},
		{"GET /v1/projects", func() (int, map[string]any) { return call("GET", "/v1/projects", nil) }},
		{"POST /v1/messages/channel", func() (int, map[string]any) {
			return call("POST", "/v1/messages/channel", map[string]any{"channel_uuid": deploys.String(), "text": "Deployed v2.72.1\n\n- api\n- web"})
		}},
		{"POST /v1/messages/dm", func() (int, map[string]any) {
			return call("POST", "/v1/messages/dm", map[string]any{"to_uuid": jonas.UserDgraphInfo.Uuid, "text": "The deploy is out."})
		}},
		{"GET /v1/search", func() (int, map[string]any) { return call("GET", "/v1/search?q=deploy", nil) }},
		{"POST /v1/search", func() (int, map[string]any) { return call("POST", "/v1/search", map[string]any{"query": "deploy"}) }},
		{"POST /v1/mcp", func() (int, map[string]any) {
			return call("POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
				"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "ci", "version": "1"}}})
		}},
	}

	mounted := map[string]bool{}
	if err := chi.Walk(routes.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/v1/") {
			mounted[method+" "+strings.TrimSuffix(route, "/")] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(mounted) == 0 {
		t.Fatal("found no /v1 routes to check")
	}
	// The MCP server is the AI edition's own: built without AI, it answers
	// 501 and says why. It is the one route that may; the six that did by
	// accident are the bug this test is for.
	aiBuilt := helpers.FeatureRegistered(helpers.FeatureNameAI)
	covered := map[string]bool{}
	for _, s := range steps {
		covered[s.route] = true
		if !mounted[s.route] {
			continue // a route this edition doesn't mount (/attention is the AI edition's)
		}
		code, out := s.run()
		if !aiBuilt && s.route == "POST /v1/mcp" {
			if code != http.StatusNotImplemented {
				t.Errorf("%s answered %d on a build without AI, want its 501: %v", s.route, code, out)
			}
			continue
		}
		if code < 200 || code > 299 {
			t.Errorf("%s answered %d: %v", s.route, code, out)
		}
	}
	for route := range mounted {
		if !covered[route] {
			t.Errorf("%s has no case here: add one, so every public route is checked", route)
		}
	}
	if taskID == "" {
		t.Error("POST /v1/tasks named no task_uuid")
	}
}
