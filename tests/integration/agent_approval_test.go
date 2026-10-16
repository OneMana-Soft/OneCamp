//go:build integration
// +build integration

package integration_test

// An approved MCP write runs once, as it was approved.
//
// WHY THIS EXISTS. An agent in approval mode proposes its writes for a person to
// approve. A write to an MCP server never ran: approval looked its tool up among
// OneCamp's own and found nothing, so every approved MCP write failed with "no
// executor registered", in front of the person who had just approved it.
//
// Running it is not the whole fix. The approval is a person's decision about
// one call, so the call that runs is held to what they decided: the same tool,
// sending to the same place, with the same arguments, for the same person, once.
// Each of those is a subtest below, against a real MCP server that records every
// call that reaches it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	// Linked for its init, as the server links it: it installs how an agent's
	// approved call is decided (aiBusiness.RegisterApprovedCallCheck).
	_ "github.com/akashc777/OneCamp/business/AIAgent"
	aimcp "github.com/akashc777/OneCamp/business/AIMCP"
	aiController "github.com/akashc777/OneCamp/controllers/AI"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// approvalWitness is an MCP server offering whatever tools_cache says, that
// records the arguments of every tool call reaching it.
type approvalWitness struct {
	*httptest.Server
	mu    sync.Mutex
	calls []map[string]any
}

func newApprovalWitness(t *testing.T) *approvalWitness {
	t.Helper()
	w := &approvalWitness{}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "notifications/initialized" {
			rw.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{}
		if req.Method == "tools/call" {
			w.mu.Lock()
			w.calls = append(w.calls, req.Params)
			w.mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Created issue 41"}}}
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(w.Close)
	return w
}

// received is every tool call that reached the server, in order.
func (w *approvalWitness) received() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.calls...)
}

func TestAnApprovedMCPWriteRunsOnceAsApproved(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// The workspace model. Nothing here asks it anything; an approval only runs
	// a tool, but it runs only while AI is on.
	ollama := httptest.NewServer(fakeOllama{})
	defer ollama.Close()
	t.Setenv("OLLAMA_HOST", ollama.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1`, fakeEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil {
		t.Fatalf("init AI: %v", err)
	}
	if !ai.GetService().IsEnabled() {
		t.Fatal("AI did not come up enabled; nothing below would run")
	}
	// The service and the tool registry are process globals: leave both as the
	// next test in this binary expects them.
	t.Cleanup(func() {
		ai.SetDynamicTools(nil, nil)
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})

	// People. The sponsor owns the agent and approves its proposals; the others
	// ask it for things. An admin adds the MCP server, which is on loopback and
	// so is reachable only for a server an admin added.
	sponsor := seedPerson(t, env.PG, "Sana")
	asker := seedPerson(t, env.PG, "Ravi")
	leaver := seedPerson(t, env.PG, "Omar")
	admin := seedPerson(t, env.PG, "Ada")
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`, admin); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sponsor", "user_uuid": sponsor, "user_name": "sana", "user_full_name": "Sana"},
		{"uid": "_:asker", "user_uuid": asker, "user_name": "ravi", "user_full_name": "Ravi"},
		{"uid": "_:leaver", "user_uuid": leaver, "user_name": "omar", "user_full_name": "Omar"},
	})

	tracker, elsewhere := newApprovalWitness(t), newApprovalWitness(t)
	serverID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_mcp_servers (id, name, url, auth_type, tool_prefix, tools_cache, created_by)
		VALUES ($1, 'Tracker', $2, 'none', 'tracker_', $3, $4)`, serverID, tracker.URL,
		`[{"name":"create_issue","description":"Create an issue","inputSchema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}]`,
		admin); err != nil {
		t.Fatalf("seed MCP server: %v", err)
	}
	if err := aimcp.RebuildRegistry(ctx); err != nil {
		t.Fatal(err)
	}
	const tool = "tracker_create_issue"
	if !ai.IsOutsideTool(tool) || ai.ToolIsReadOnly(tool) {
		t.Fatalf("%s should be registered as a write from an MCP server", tool)
	}

	// The agent: in approval mode, given the tool, so its writes wait for its
	// sponsor.
	agentID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, autonomy, enabled_tools)
		VALUES ($1, 'Tracker bot', $2, 'mention', 'approval', $3)`, agentID, sponsor, `["`+tool+`"]`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// propose files a write the way the agent's runner does in approval mode:
	// in a run for whoever asked (or for the sponsor alone when askedBy is
	// empty), for the sponsor to approve.
	propose := func(t *testing.T, askedBy, title string, attr pendingModels.Attribution) *pendingModels.PendingAction {
		t.Helper()
		runCtx := ai.WithoutRunRequester(ctx)
		if askedBy != "" {
			runCtx = ai.WithRunRequester(ctx, askedBy, sponsor)
		}
		a, err := aiBusiness.CreatePendingAction(runCtx, uuid.MustParse(sponsor), "agent", agentID.String(), tool,
			map[string]string{"title": title}, "Tracker bot: create an issue", "", attr)
		if err != nil {
			t.Fatalf("propose: %v", err)
		}
		return a
	}
	byAgent := pendingModels.Attribution{AgentID: &agentID}

	// approve is the sponsor pressing Approve: the endpoint, with the id alone.
	approver := userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: uuid.MustParse(sponsor)},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: sponsor},
	}
	approve := func(t *testing.T, id uuid.UUID) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id.String())
		reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
		reqCtx = context.WithValue(reqCtx, helpers.UserInfoContextKey, approver)
		rec := httptest.NewRecorder()
		aiController.ApprovePendingAction(rec, req.WithContext(reqCtx))
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Data
	}
	outcome := func(data map[string]any) (status, result, failure string) {
		status, _ = data["status"].(string)
		result, _ = data["result"].(string)
		failure, _ = data["error"].(string)
		return
	}

	first := propose(t, asker, "Fix the login page", byAgent)

	t.Run("approved, the write runs once, with the arguments that were proposed", func(t *testing.T) {
		if first.AskedBy == nil || first.AskedBy.String() != asker || first.ToolBinding == "" {
			t.Fatalf("the proposal must be bound to who asked and to where its tool sends it: %+v", first)
		}
		code, data := approve(t, first.Id)
		status, result, failure := outcome(data)
		if code != http.StatusOK || status != pendingModels.StatusExecuted {
			t.Fatalf("approval: HTTP %d, status %q, error %q; want 200 and %q", code, status, failure, pendingModels.StatusExecuted)
		}
		if result != "Created issue 41" {
			t.Errorf("the result should be what the server answered; got %q", result)
		}
		calls := tracker.received()
		if len(calls) != 1 {
			t.Fatalf("the server should have been called once; got %d calls", len(calls))
		}
		args, _ := calls[0]["arguments"].(map[string]any)
		if calls[0]["name"] != "create_issue" || args["title"] != "Fix the login page" || len(args) != 1 {
			t.Fatalf("the call must be the proposed one, tool and arguments alike; got %v", calls[0])
		}
	})

	t.Run("approving it again is refused, and nothing runs twice", func(t *testing.T) {
		code, data := approve(t, first.Id)
		if status, _, _ := outcome(data); code != http.StatusConflict || status != pendingModels.StatusExecuted {
			t.Fatalf("a replayed approval: HTTP %d, status %q; want 409 and the action as it ended", code, status)
		}
		if n := len(tracker.received()); n != 1 {
			t.Fatalf("a replayed approval must not call the server again; it has %d calls", n)
		}
	})

	t.Run("it runs only while the person it was for could still ask for it", func(t *testing.T) {
		before := len(tracker.received())
		pending := propose(t, leaver, "Archive the old board", byAgent)
		// Omar leaves between the proposal and the approval.
		gone := time.Now().UTC().Format(time.RFC3339)
		dg.Mutate(t, []map[string]any{{"uid": uids["leaver"], "user_deleted_at": gone}})
		if _, err := env.PG.Exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, leaver); err != nil {
			t.Fatal(err)
		}
		code, data := approve(t, pending.Id)
		status, _, failure := outcome(data)
		if code != http.StatusOK || status != pendingModels.StatusFailed || !strings.Contains(failure, "wasn't run") {
			t.Fatalf("approval for someone who has left: HTTP %d, status %q, error %q; want it refused", code, status, failure)
		}
		if n := len(tracker.received()); n != before {
			t.Fatalf("nothing may be sent for someone who has left; the server got %d more call(s)", n-before)
		}
	})

	t.Run("a call whose server has moved since it was proposed is not sent", func(t *testing.T) {
		before := len(tracker.received())
		pending := propose(t, "", "Close the stale issues", byAgent)
		if _, err := env.PG.Exec(`UPDATE ai_mcp_servers SET url = $2, updated_at = now() WHERE id = $1`, serverID, elsewhere.URL); err != nil {
			t.Fatal(err)
		}
		if err := aimcp.RebuildRegistry(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = env.PG.Exec(`UPDATE ai_mcp_servers SET url = $2 WHERE id = $1`, serverID, tracker.URL)
			_ = aimcp.RebuildRegistry(context.Background())
		})
		code, data := approve(t, pending.Id)
		status, _, failure := outcome(data)
		if code != http.StatusOK || status != pendingModels.StatusFailed || !strings.Contains(failure, "changed since it was proposed") {
			t.Fatalf("approval after the server moved: HTTP %d, status %q, error %q; want it refused", code, status, failure)
		}
		if n := len(tracker.received()) - before; n != 0 || len(elsewhere.received()) != 0 {
			t.Fatalf("neither address may be called: the old got %d, the new %d", n, len(elsewhere.received()))
		}
	})

	t.Run("a switched-off agent's proposal doesn't run", func(t *testing.T) {
		before := len(tracker.received())
		pending := propose(t, "", "Label the new issues", byAgent)
		if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, agentID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = env.PG.Exec(`UPDATE ai_agents SET is_active = true WHERE id = $1`, agentID) })
		code, data := approve(t, pending.Id)
		status, _, failure := outcome(data)
		if code != http.StatusOK || status != pendingModels.StatusFailed || !strings.Contains(failure, "switched off") {
			t.Fatalf("approval for a switched-off agent: HTTP %d, status %q, error %q; want it refused", code, status, failure)
		}
		if n := len(tracker.received()) - before; n != 0 {
			t.Fatalf("a switched-off agent's proposal must not run; the server got %d call(s)", n)
		}
	})

	t.Run("a proposal no agent made never reaches an MCP tool", func(t *testing.T) {
		before := len(tracker.received())
		pending := propose(t, "", "Delete the backlog", pendingModels.Attribution{})
		code, data := approve(t, pending.Id)
		status, _, failure := outcome(data)
		if code != http.StatusOK || status != pendingModels.StatusFailed || !strings.Contains(failure, "no executor") {
			t.Fatalf("approval of an MCP tool nobody gave to an agent: HTTP %d, status %q, error %q; want it refused", code, status, failure)
		}
		if n := len(tracker.received()) - before; n != 0 {
			t.Fatalf("only an agent given an MCP tool may run it; the server got %d call(s)", n)
		}
	})
}
