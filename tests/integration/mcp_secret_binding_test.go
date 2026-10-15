//go:build integration
// +build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	agentController "github.com/akashc777/OneCamp/controllers/AIAgent"
	mcpController "github.com/akashc777/OneCamp/controllers/AIMCP"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	mcpModel "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// secretWitness is a test server that answers MCP and AG-UI just enough, and
// records every credential that reached it.
type secretWitness struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []string
	calls int
}

func newSecretWitness(t *testing.T) *secretWitness {
	t.Helper()
	w := &secretWitness{}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.calls++
		for _, h := range []string{"Authorization", "X-Api-Key", "X-Token"} {
			if v := r.Header.Get(h); v != "" {
				w.seen = append(w.seen, h+": "+v)
			}
		}
		w.mu.Unlock()
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "":
			rw.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(rw, "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"a\",\"delta\":\"hi\"}\n\ndata: {\"type\":\"RUN_FINISHED\"}\n\n")
		case "notifications/initialized":
			rw.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(rw).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "push_files"}}}})
		default:
			_ = json.NewEncoder(rw).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *secretWitness) credentials() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

func (w *secretWitness) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// call runs a controller as who, with chi's {id}.
func call(t *testing.T, h http.HandlerFunc, who userModels.UserInfo, id string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	ctx = context.WithValue(ctx, helpers.UserInfoContextKey, who)
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func seedPerson(t *testing.T, env *Env, admin bool) userModels.UserInfo {
	t.Helper()
	id := uuid.New()
	email := "p-" + id.String()[:8] + "@example.com"
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if admin {
		if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, email); err != nil {
			t.Fatalf("seed admin: %v", err)
		}
	}
	return userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email, IsAdmin: admin}}
}

// The hole this closes: one save moved an MCP server to another address and
// the stored token went with it. Now a kept token never reaches a new host,
// only its creator or an admin may change, switch, test or remove a server,
// and a remote agent's secret is held to the same rule.
func TestAStoredSecretNeverReachesANewAddress(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "integration-ai-kek-0123456789abcdef")
	env := SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	admin := seedPerson(t, env, true)
	member := seedPerson(t, env, false)
	// The witnesses listen on loopback: this server's own network, which only
	// a server or agent an admin added may reach.
	home, elsewhere := newSecretWitness(t), newSecretWitness(t)

	// A member may not add one there; nobody may add a metadata address.
	code, out := call(t, mcpController.CreateServer, member, "", map[string]any{
		"name": "Inside", "url": home.URL + "/mcp", "auth_type": "none", "enabled": true,
	})
	if code != http.StatusBadRequest || !strings.Contains(out["msg"].(string), "Only an admin can add") {
		t.Fatalf("a member's server on this server's network: %d %v", code, out)
	}
	code, out = call(t, mcpController.CreateServer, admin, "", map[string]any{
		"name": "Metadata", "url": "http://169.254.169.254/latest/meta-data/", "auth_type": "none", "enabled": true,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("an admin's server at the metadata address: %d %v", code, out)
	}
	if home.callCount() != 0 {
		t.Fatal("a refused server was reached")
	}

	code, out = call(t, mcpController.CreateServer, admin, "", map[string]any{
		"name": "GitHub", "url": home.URL + "/mcp", "auth_type": "bearer", "auth_secret": "admin-token", "enabled": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	waitFor(t, "the first introspection", func() bool { return home.callCount() > 0 })

	// Moving the address, or how the token is attached, without entering it
	// again: 400, in words, and the new address hears nothing.
	for _, change := range []map[string]any{
		{"name": "GitHub", "url": elsewhere.URL + "/mcp", "auth_type": "bearer", "enabled": true},
		{"name": "GitHub", "url": home.URL + "/mcp", "auth_type": "header", "auth_header_name": "X-Api-Key", "enabled": true},
	} {
		code, out = call(t, mcpController.UpdateServer, admin, id, change)
		if code != http.StatusBadRequest || out["code"] != "secret_reentry" || out["msg"] != "Enter the token again for the new address." {
			t.Fatalf("a kept token moving: %d %v", code, out)
		}
	}
	srv, _ := mcpModel.GetServerByID(context.Background(), uuid.MustParse(id))
	if srv == nil || !strings.HasPrefix(srv.URL, home.URL) || srv.AuthType != "bearer" {
		t.Fatalf("the refused save changed the row: %+v", srv)
	}

	// Another member may do none of it, whatever they send.
	for name, h := range map[string]http.HandlerFunc{
		"update": mcpController.UpdateServer, "enabled": mcpController.SetEnabled,
		"delete": mcpController.DeleteServer, "test": mcpController.TestConnection,
	} {
		code, out = call(t, h, member, id, map[string]any{
			"name": "GitHub", "url": home.URL + "/mcp", "auth_type": "bearer", "enabled": false,
		})
		if code != http.StatusForbidden {
			t.Errorf("a member's %s: %d %v; want 403", name, code, out)
		}
	}
	code, out = call(t, mcpController.GetServer, member, id, nil)
	if code != http.StatusOK || out["data"].(map[string]any)["can_manage"] != false {
		t.Errorf("a member reads it, and is told they can't manage it: %d %v", code, out)
	}

	// The admin moves it properly, with a token for the new address.
	code, out = call(t, mcpController.UpdateServer, admin, id, map[string]any{
		"name": "GitHub", "url": elsewhere.URL + "/mcp", "auth_type": "bearer", "auth_secret": "elsewhere-token",
		"update_secret": true, "enabled": true,
	})
	if code != http.StatusOK {
		t.Fatalf("a move with a new token: %d %v", code, out)
	}
	code, out = call(t, mcpController.TestConnection, admin, id, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("test after the move: %d %v", code, out)
	}
	for _, c := range elsewhere.credentials() {
		if strings.Contains(c, "admin-token") {
			t.Fatalf("the old token reached the new address: %v", elsewhere.credentials())
		}
	}
	if len(elsewhere.credentials()) == 0 {
		t.Fatal("the new address got no token at all")
	}

	// A remote agent's secret, by the same rule. Its new address is a witness
	// of its own: the MCP server moved above keeps being introspected in the
	// background, and those calls are its own token going where it now lives.
	elsewhere = newSecretWitness(t)
	agentID, err := agentModel.CreateAgent(context.Background(), &agentModel.AiAgent{
		Name: "Remote", Instructions: "x", TriggerType: agentModel.TriggerManual, MaxSteps: 1,
		AGUIEndpoint: home.URL + "/agent", AGUIAuthHeader: "X-Token", AGUIAuthSecret: "agent-secret",
		RemoteProtocol: agentModel.RemoteAGUI, CreatedBy: admin.UserPostgresInfo.Id,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	before := elsewhere.callCount()
	code, out = call(t, agentController.UpdateAgent, admin, agentID.String(), map[string]any{
		"name": "Remote", "instructions": "x", "trigger_type": "manual", "max_steps": 1,
		"agui_endpoint": elsewhere.URL + "/agent", "agui_auth_header": "X-Token",
	})
	if code != http.StatusBadRequest || out["code"] != "secret_reentry" {
		t.Fatalf("an agent moved without its secret: %d %v", code, out)
	}
	code, out = call(t, agentController.CheckRemoteBrain, admin, "", agentBusiness.RemoteBrainCheck{
		AgentID: agentID.String(), Endpoint: elsewhere.URL + "/agent", AuthHeader: "X-Token",
	})
	if code != http.StatusBadRequest || out["code"] != "secret_reentry" {
		t.Fatalf("a check of another address with the stored secret: %d %v", code, out)
	}
	code, out = call(t, agentController.CheckRemoteBrain, member, "", agentBusiness.RemoteBrainCheck{
		AgentID: agentID.String(), Endpoint: home.URL + "/agent", AuthHeader: "X-Token",
	})
	if code != http.StatusForbidden {
		t.Fatalf("a member checking an admin's agent with its secret: %d %v", code, out)
	}
	if elsewhere.callCount() != before {
		t.Fatalf("the new address was called %d times", elsewhere.callCount()-before)
	}
	code, out = call(t, agentController.CheckRemoteBrain, admin, "", agentBusiness.RemoteBrainCheck{
		AgentID: agentID.String(), Endpoint: home.URL + "/agent", AuthHeader: "x-token",
	})
	if code != http.StatusOK {
		t.Fatalf("a check of the agent's own address keeps its secret: %d %v", code, out)
	}
	for _, c := range elsewhere.credentials() {
		if strings.Contains(c, "agent-secret") {
			t.Fatalf("the agent's secret reached the new address: %v", c)
		}
	}
}
