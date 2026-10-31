//go:build integration
// +build integration

package integration_test

// The work graph's agent tools (DG8) take a folder, a file and a link (docs
// F1, DGF1) over MCP, with an API token, against real Postgres 12, Redis and
// Dgraph with the production schema.
//
//   - olu made a workspace folder, Finance, with a file (Q3 budget.xlsx) and
//     a link (Forecast) in it; a private folder, Vault, with a private file
//     in it (Salaries.xlsx); and a private page, Secret;
//   - mia is a member, with a token, who can open Finance and what's in it,
//     and nothing private.
//
// The rules held: an answer names a folder, a file and a link by what they
// are, and an agent that names one by that word is answered as the doc it
// is; a folder or a file the caller can't open is refused in the same words
// as the same uuid named as a doc (the doc's own rule decides both, never a
// looser one), and every refusal is audited.
//
// AI edition only (business/AI's executors, business/MCPServer's bridge).
//
// Run: integ_rel.sh <wt> <log> '^TestGraphToolsTakeFoldersOverMCP$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGraphToolsTakeFoldersOverMCP(t *testing.T) {
	t.Setenv("JWT_SECRET", "graph-tools-folders-integration-secret")
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
	for _, n := range []string{"olu", "mia"} {
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
	olu := map[string]any{"uid": people["olu"].UserDgraphInfo.Uid}

	zero := "0001-01-01T00:00:00Z"
	id := map[string]string{}
	for _, k := range []string{"fin", "q3", "link1", "vault", "sal", "secret"} {
		id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	doc := func(key, title string, private bool, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": zero, "doc_owner": olu, "doc_created_by": olu}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	dg.Mutate(t, []map[string]any{
		doc("fin", "Finance", false, map[string]any{"doc_node_kind": "folder", "doc_color": "moss"}),
		doc("q3", "Q3 budget.xlsx", false, map[string]any{"doc_node_kind": "file", "doc_parent": ref("fin")}),
		doc("link1", "Forecast", false, map[string]any{"doc_node_kind": "link", "doc_color": "sky",
			"doc_link_url": "https://example.com/forecast", "doc_parent": ref("fin")}),
		doc("vault", "Vault", true, map[string]any{"doc_node_kind": "folder"}),
		doc("sal", "Salaries.xlsx", true, map[string]any{"doc_node_kind": "file", "doc_parent": ref("vault")}),
		doc("secret", "Secret", true, nil),
	})

	tok, err := apiTokenBusiness.CreateToken(ctx, "mia's script", []string{apiTokenBusiness.ScopeSearchRead}, 30,
		people["mia"].UserPostgresInfo.Id, nil, false)
	if err != nil {
		t.Fatalf("make mia's token: %v", err)
	}
	miaTok := tok.Plaintext
	routes := router.Routes()
	// tool calls an MCP tool over /v1/mcp with a token, and answers its text,
	// or "refused: <why>".
	tool := func(name string, args map[string]any) (text, refusal string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
		req := httptest.NewRequest("POST", "/v1/mcp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+miaTok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
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
	mia := people["mia"].UserPostgresInfo.Id.String()
	refusals := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = $1 AND metadata->>'tool' = $2 AND metadata->>'principal_user_id' = $3`,
			mcpBusiness.ActionToolCallRefused, "related_items", mia).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	named := func(kind, key, title string) string { return kind + ` "` + title + `" (` + id[key] + `)` }

	t.Run("a folder, a file and a link answer as the doc they are", func(t *testing.T) {
		text, refused := tool("related_items", map[string]any{"kind": "folder", "uuid": id["fin"]})
		for _, want := range []string{"Connected to " + named("folder", "fin", "Finance"), "contains " + named("file", "q3", "Q3 budget.xlsx"),
			"contains " + named("link", "link1", "Forecast")} {
			if refused != "" || !strings.Contains(text, want) {
				t.Errorf("mia, the folder: lacks %q (%s)\n%s", want, refused, text)
			}
		}
		text, refused = tool("related_items", map[string]any{"kind": "file", "uuid": id["q3"]})
		if refused != "" || !strings.Contains(text, "Connected to "+named("file", "q3", "Q3 budget.xlsx")) || !strings.Contains(text, "in "+named("folder", "fin", "Finance")) {
			t.Errorf("mia, the file: %s\n%s", refused, text)
		}
		text, refused = tool("related_items", map[string]any{"kind": "Link", "uuid": id["link1"]})
		if refused != "" || !strings.Contains(text, "Connected to "+named("link", "link1", "Forecast")) || !strings.Contains(text, "in "+named("folder", "fin", "Finance")) {
			t.Errorf("mia, the link: %s\n%s", refused, text)
		}
		if strings.Contains(text, "example.com") {
			t.Errorf("an answer carries the link's address:\n%s", text)
		}
		// Two ways of the same length join them (the folder, and olu who made
		// both): which comes back follows the graph's uid order, so only the
		// answer's ends and that there is a way are held here.
		text, refused = tool("how_connected", map[string]any{"from_kind": "file", "from_uuid": id["q3"], "to_kind": "link", "to_uuid": id["link1"]})
		if refused != "" || !strings.Contains(text, "How "+named("file", "q3", "Q3 budget.xlsx")+" connects to "+named("link", "link1", "Forecast")) ||
			!strings.Contains(text, "\n1. ") {
			t.Errorf("mia, the file to the link: %s\n%s", refused, text)
		}
		for _, key := range []string{"vault", "sal", "secret"} {
			if strings.Contains(text, id[key]) {
				t.Errorf("an answer names %s:\n%s", key, text)
			}
		}
	})

	t.Run("a private folder or file is refused as the same doc is, and every refusal is audited", func(t *testing.T) {
		before := refusals()
		for _, c := range []struct{ word, key string }{{"folder", "vault"}, {"file", "sal"}} {
			_, asWord := tool("related_items", map[string]any{"kind": c.word, "uuid": id[c.key]})
			_, asDoc := tool("related_items", map[string]any{"kind": "doc", "uuid": id[c.key]})
			if asWord == "" || asWord != asDoc {
				t.Errorf("the %s named %q is refused as %q, named a doc as %q", c.key, c.word, asWord, asDoc)
			}
			if strings.Contains(asWord, "Vault") || strings.Contains(asWord, "Salaries") {
				t.Errorf("the refusal names what it protects: %s", asWord)
			}
		}
		// The private page, for the doc's own words.
		_, page := tool("related_items", map[string]any{"kind": "doc", "uuid": id["secret"]})
		if page == "" {
			t.Error("mia read what the private page is connected to")
		}
		if after := refusals(); after != before+5 {
			t.Errorf("%d refusals audited, want %d", after, before+5)
		}
	})
}
