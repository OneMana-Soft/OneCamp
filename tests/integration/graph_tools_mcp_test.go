//go:build integration
// +build integration

package integration_test

// The work graph's agent tools (DG8), related_items and how_connected,
// through the real MCP path (POST /v1/mcp on the router, the governed
// ladder, the audit) and through an in-app agent run asked by someone else,
// against real Postgres 12, Dgraph and Redis.
//
//   - olu owns a workspace doc (Plan), a private doc (Secret) that Plan
//     links, and a workspace doc (Brief) reached only through Secret; a task
//     in project P links Plan; a post in the public #launch shares Plan;
//   - mia is a member: in P, in #launch;
//   - out is in the workspace and nothing else.
//
// What holds: a token reads as the member it belongs to, exactly what the
// Connected panel shows them; a private doc in the middle of a path is never
// named, and no path through it is given; a start the person can't open is
// refused, the refusal audited; a graph uid is refused; a paused agent's
// credential is refused before anything is read; and an agent run for
// someone other than its sponsor answers with what both may see.
//
// Run: integ_rel.sh <wt> <log> '^TestGraphToolsOverMCP$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	_ "github.com/akashc777/OneCamp/business/AI"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workGraphController "github.com/akashc777/OneCamp/controllers/WorkGraph"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGraphToolsOverMCP(t *testing.T) {
	t.Setenv("JWT_SECRET", "graph-tools-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
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
	if err := mcpBusiness.SetAdmission(ctx, true, "*"); err != nil {
		t.Fatalf("turn MCP on: %v", err)
	}
	if err := mcpBusiness.RegisterBridgedTools(); err != nil && !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("register the MCP tools: %v", err)
	}

	people := map[string]userModels.UserInfo{}
	for _, n := range []string{"olu", "mia", "out"} {
		id, _, err := userBusiness.CreateUserWithMethod(ctx, n+"@example.test", n, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatalf("make %s: %v", n, err)
		}
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", n, err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || node == nil {
			t.Fatalf("load %s's node: %v", n, err)
		}
		people[n] = userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
	}
	uidOf := func(n string) map[string]any { return map[string]any{"uid": people[n].UserDgraphInfo.Uid} }

	zero := "0001-01-01T00:00:00Z"
	id := map[string]string{}
	for _, k := range []string{"plan", "secret", "brief", "proj", "team", "task", "launch", "m1"} {
		id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	doc := func(key, title string, private bool, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": zero, "doc_owner": uidOf("olu"), "doc_created_by": uidOf("olu")}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	dg.Mutate(t, []map[string]any{
		doc("plan", "Plan", false, map[string]any{"doc_mentions": []map[string]any{ref("secret")}}),
		doc("secret", "Secret", true, map[string]any{"doc_mentions": []map[string]any{ref("brief")}}),
		// Brief is out's, so the only way from Plan to it is through Secret.
		doc("brief", "Brief", false, map[string]any{"doc_owner": uidOf("out"), "doc_created_by": uidOf("out")}),
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": id["team"], "team_name": "Launch team", "team_deleted_at": zero,
			"team_members": []map[string]any{uidOf("olu"), uidOf("mia")}, "team_projects": []map[string]any{ref("proj")}},
		{"uid": "_:proj", "dgraph.type": "Project", "project_uuid": id["proj"], "project_name": "P", "project_deleted_at": zero,
			"project_team": ref("team"), "project_members": []map[string]any{uidOf("olu"), uidOf("mia")}, "project_admins": []map[string]any{uidOf("olu")},
			"project_tasks": []map[string]any{ref("task")}},
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": id["task"], "task_name": "Record the film", "task_status": "todo",
			"task_deleted_at": zero, "task_project": ref("proj"), "task_created_by": uidOf("olu"), "task_assignee": uidOf("mia"),
			"linked_docs": []map[string]any{ref("plan")}},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": id["launch"], "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{uidOf("olu"), uidOf("mia")}, "ch_posts": []map[string]any{ref("m1")}},
		{"uid": "_:m1", "dgraph.type": "Post", "post_uuid": id["m1"], "post_text": "<p>The plan is up</p>", "post_deleted_at": zero,
			"post_created_at": "2026-10-10T10:00:00Z", "post_by": uidOf("olu"), "post_channel": ref("launch"), "post_doc": ref("plan")},
	})
	name := map[string]string{}
	for k, v := range id {
		name[v] = k
	}
	for n, u := range people {
		name[u.UserPostgresInfo.Id.String()] = n
	}
	// named is which of the fixture's things an answer names, sorted.
	named := func(text string) []string {
		var out []string
		for v, k := range name {
			if strings.Contains(text, v) {
				out = append(out, k)
			}
		}
		return sortedCopy(out)
	}

	token := func(who string, agentID *uuid.UUID) string {
		t.Helper()
		tok, err := apiTokenBusiness.CreateToken(ctx, who+"'s script", []string{apiTokenBusiness.ScopeSearchRead}, 30,
			people[who].UserPostgresInfo.Id, agentID, false)
		if err != nil {
			t.Fatalf("make %s's token: %v", who, err)
		}
		return tok.Plaintext
	}
	routes := router.Routes()
	// tool calls an MCP tool over /v1/mcp with a token, and answers its text,
	// or "refused: <why>".
	tool := func(bearer, name string, args map[string]any) (text, refusal string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		req := httptest.NewRequest("POST", "/v1/mcp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			// The /v1 token middleware refuses a credential before MCP sees it
			// (a revoked token, a paused agent: BoundAgent).
			return "", fmt.Sprintf("refused with %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
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
	refusals := func(tool, principal string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = $1 AND metadata->>'tool' = $2 AND metadata->>'principal_user_id' = $3`,
			mcpBusiness.ActionToolCallRefused, tool, principal).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// panel is what the Connected panel shows who, in the app, signed in.
	panel := func(who, kind, key string, depth int) []string {
		t.Helper()
		q := url.Values{"kind": {kind}, "uuid": {id[key]}, "depth": {fmt.Sprint(depth)}}
		req := httptest.NewRequest("GET", "/graph/connected?"+q.Encode(), nil)
		c := context.WithValue(ctx, helpers.UserInfoContextKey, people[who])
		rec := httptest.NewRecorder()
		workGraphController.GetConnected(rec, req.WithContext(c))
		if rec.Code != 200 {
			t.Fatalf("%s's panel: %d %s", who, rec.Code, rec.Body.String())
		}
		return named(rec.Body.String())
	}

	miaTok := token("mia", nil)

	t.Run("a token reads as its member: what their Connected panel shows, no more", func(t *testing.T) {
		text, refused := tool(miaTok, "related_items", map[string]any{"kind": "doc", "uuid": id["plan"], "depth": "2"})
		if refused != "" {
			t.Fatal(refused)
		}
		got, want := named(text), panel("mia", "doc", "plan", 2)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the token names %v, the member's panel %v\n%s", got, want, text)
		}
		for _, k := range []string{"task", "proj", "m1", "launch"} {
			if !strings.Contains(text, id[k]) {
				t.Errorf("the member's answer leaves out %s:\n%s", k, text)
			}
		}
		if strings.Contains(text, id["secret"]) || strings.Contains(text, id["brief"]) || strings.Contains(text, "Secret") {
			t.Errorf("the member's answer names the private doc or what lies past it:\n%s", text)
		}
		// The owner, through their own token, sees past it: the test can tell.
		olu, _ := tool(token("olu", nil), "related_items", map[string]any{"kind": "doc", "uuid": id["plan"], "depth": "2"})
		if !strings.Contains(olu, id["secret"]) || !strings.Contains(olu, id["brief"]) {
			t.Errorf("the owner doesn't see the private doc and past it:\n%s", olu)
		}
	})

	t.Run("a private doc in the middle of a path is never named", func(t *testing.T) {
		text, refused := tool(miaTok, "how_connected", map[string]any{"from_kind": "doc", "from_uuid": id["plan"], "to_kind": "doc", "to_uuid": id["brief"]})
		if refused != "" {
			t.Fatal(refused)
		}
		if strings.Contains(text, id["secret"]) || strings.Contains(text, "Secret") || !strings.Contains(text, "No connection you can see") {
			t.Errorf("mia, Plan to Brief:\n%s", text)
		}
		olu, _ := tool(token("olu", nil), "how_connected", map[string]any{"from_kind": "doc", "from_uuid": id["plan"], "to_kind": "doc", "to_uuid": id["brief"]})
		if !strings.Contains(olu, id["secret"]) {
			t.Errorf("olu, Plan to Brief, goes through Secret:\n%s", olu)
		}
		// A path the member can see is given whole.
		p, _ := tool(miaTok, "how_connected", map[string]any{"from_kind": "doc", "from_uuid": id["plan"], "to_kind": "project", "to_uuid": id["proj"]})
		if !strings.Contains(p, id["task"]) || !strings.Contains(p, id["proj"]) {
			t.Errorf("mia, Plan to P:\n%s", p)
		}
	})

	t.Run("a start the person can't open is refused, and the refusal is audited", func(t *testing.T) {
		out := people["out"].UserPostgresInfo.Id.String()
		before := refusals("related_items", out)
		_, refused := tool(token("out", nil), "related_items", map[string]any{"kind": "doc", "uuid": id["secret"]})
		if refused == "" {
			t.Fatal("out read what the private doc is connected to")
		}
		if strings.Contains(refused, "Secret") || strings.Contains(refused, id["brief"]) {
			t.Errorf("the refusal names what it protects: %s", refused)
		}
		_, refused = tool(token("out", nil), "related_items", map[string]any{"kind": "project", "uuid": id["proj"]})
		if refused == "" {
			t.Error("out read a project they aren't in")
		}
		if after := refusals("related_items", out); after != before+2 {
			t.Errorf("%d refusals audited, want %d", after, before+2)
		}
		// A person, a kind with no rule of its own here, is checked by the work graph's.
		if text, refused := tool(miaTok, "related_items", map[string]any{"kind": "person", "uuid": people["olu"].UserPostgresInfo.Id.String()}); refused != "" || !strings.Contains(text, id["plan"]) {
			t.Errorf("mia, what olu is connected to: %q %q", text, refused)
		}
	})

	t.Run("only uuids and kinds", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"kind": "doc", "uuid": people["olu"].UserDgraphInfo.Uid},
			{"kind": "doc", "uuid": `{ q(func: has(doc_uuid)) { uid doc_title } }`},
			{"kind": "nonsense", "uuid": id["plan"]},
		} {
			if text, refused := tool(miaTok, "related_items", args); refused == "" {
				t.Errorf("%v answered: %s", args, text)
			}
		}
	})

	t.Run("a paused agent's credential is refused before anything is read", func(t *testing.T) {
		agent := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, enabled_tools)
			VALUES ($1, 'Mapper', $2, 'mention', '["related_items","how_connected"]'::jsonb)`, agent, people["mia"].UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		bound := token("mia", &agent)
		if text, refused := tool(bound, "related_items", map[string]any{"kind": "doc", "uuid": id["plan"]}); refused != "" || !strings.Contains(text, id["task"]) {
			t.Fatalf("the live agent: %q %q", text, refused)
		}
		if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, agent); err != nil {
			t.Fatal(err)
		}
		// Refused at the door by the /v1 token middleware's BoundAgent, before
		// the MCP ladder runs, so nothing is read (the MCP refusal audit is not
		// reached; AuthorizeToolCall's own BoundAgent rung is the second line).
		text, refused := tool(bound, "related_items", map[string]any{"kind": "doc", "uuid": id["plan"]})
		if refused == "" || !strings.Contains(refused, "deactivated") || strings.Contains(refused, id["task"]) || text != "" {
			t.Errorf("the paused agent: %q %q", text, refused)
		}
		// The MCP ladder refuses it as well, and audits that.
		mia := people["mia"].UserPostgresInfo.Id.String()
		before := refusals("related_items", mia)
		d := mcpBusiness.AuthorizeToolCall(ctx, mcpBusiness.ToolCallRequest{TokenPlaintext: bound, ToolName: "related_items",
			Args: map[string]any{"kind": "doc", "uuid": id["plan"]}})
		if d.Allow || !strings.Contains(d.Reason, "deactivated") {
			t.Errorf("the ladder let the paused agent through: %+v", d.Reason)
		}
		if err := mcpBusiness.RecordDecision(ctx, d, "related_items", mcpBusiness.AuditContext{}); err != nil {
			t.Fatal(err)
		}
		if after := refusals("related_items", mia); after != before+1 {
			t.Errorf("%d refusals audited for the paused agent, want %d", after, before+1)
		}
	})

	t.Run("an agent asked by someone else answers with what both may see", func(t *testing.T) {
		run, ok := ai.GetExecutor("related_items")
		if !ok {
			t.Fatal("related_items has no executor")
		}
		olu, mia := people["olu"].UserPostgresInfo.Id.String(), people["mia"].UserPostgresInfo.Id.String()
		action := ai.ProposedAction{ToolName: "related_items", Params: map[string]string{"kind": "doc", "uuid": id["plan"], "depth": "2"}}
		// olu's agent, asked by olu: everything olu sees.
		alone, _, err := run(ai.WithRunRequester(ctx, olu, olu), action, olu)
		if err != nil || !strings.Contains(alone, id["secret"]) {
			t.Fatalf("olu's own run: %v\n%s", err, alone)
		}
		// olu's agent, asked by mia: never olu's private doc, nor past it.
		forMia, _, err := run(ai.WithRunRequester(ctx, mia, olu), action, olu)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(forMia, id["secret"]) || strings.Contains(forMia, id["brief"]) || !strings.Contains(forMia, id["task"]) {
			t.Errorf("olu's agent, asked by mia:\n%s", forMia)
		}
		// Asked by someone who can't be identified: nothing is read.
		if text, _, err := run(ai.WithRunRequester(ctx, "", olu), action, olu); err == nil {
			t.Errorf("an unidentified asker got: %s", text)
		}
		// The start is gated for the asker too, by the same rule as MCP.
		ref, known, err := mcpBusiness.ResourceForToolCall("related_items", map[string]string{"kind": "doc", "uuid": id["secret"]})
		if !known || err != nil {
			t.Fatalf("%v %v", known, err)
		}
		if d := mcpBusiness.PrincipalCanReach(ctx, mia, ref); d.Allow {
			t.Error("mia may start at olu's private doc")
		}
	})
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
