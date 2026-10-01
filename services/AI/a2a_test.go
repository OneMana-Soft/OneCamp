package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// a2aAgent is a fake A2A agent: it publishes a card at the well-known path and
// answers JSON-RPC with whatever reply returns for the method it was sent.
func a2aAgent(t *testing.T, protocol string, reply func(method string, params map[string]any, calls int32) any) *httptest.Server {
	t.Helper()
	var calls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "Release Notes Writer", "description": "Writes release notes", "url": srv.URL + "/rpc",
				"version": "2.1", "protocolVersion": protocol, "preferredTransport": "JSONRPC",
				"provider": map[string]any{"organization": "Acme"},
				"skills":   []any{map[string]any{"id": "notes", "name": "Release notes", "description": "From a changelog"}},
			})
			return
		}
		if r.URL.Path != "/rpc" || r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": reply(req.Method, req.Params, n)})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server) *A2AProvider {
	t.Helper()
	card, err := FetchA2ACard(context.Background(), srv.URL, "", "s3cret")
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if card.Name != "Release Notes Writer" || card.Provider != "Acme" || len(card.Skills) != 1 || !strings.HasSuffix(card.Endpoint, "/rpc") {
		t.Fatalf("card read wrong: %+v", card)
	}
	return NewA2AProvider(card.Endpoint, "", "s3cret", "run-1", card.ProtocolVersion)
}

func TestA2A03AnswersWithAMessage(t *testing.T) {
	srv := a2aAgent(t, "0.3.0", func(method string, p map[string]any, _ int32) any {
		msg := p["message"].(map[string]any)
		if method != "message/send" || msg["role"] != "user" || msg["kind"] != "message" || msg["contextId"] != "run-1" {
			t.Errorf("0.3 request in the wrong dialect: %s %v", method, msg)
		}
		return map[string]any{"kind": "message", "role": "agent", "parts": []any{map[string]any{"kind": "text", "text": "Drafted."}}}
	})
	got, calls, err := dial(t, srv).ChatWithTools(context.Background(), []ChatMessage{{Role: "system", Content: "tools..."}, {Role: "user", Content: "Write the notes"}}, nil, ChatOptions{})
	if err != nil || got != "Drafted." || len(calls) != 0 {
		t.Fatalf("got %q %v %v", got, calls, err)
	}
}

func TestA2A1WaitsForItsTaskToFinish(t *testing.T) {
	srv := a2aAgent(t, "1.0", func(method string, p map[string]any, n int32) any {
		switch {
		case n == 1:
			msg := p["message"].(map[string]any)
			if method != "SendMessage" || msg["role"] != "ROLE_USER" || msg["kind"] != nil {
				t.Errorf("1.0 request in the wrong dialect: %s %v", method, msg)
			}
			return map[string]any{"task": map[string]any{"id": "t1", "status": map[string]any{"state": "TASK_STATE_WORKING"}}}
		default:
			if method != "GetTask" || p["id"] != "t1" {
				t.Errorf("poll in the wrong dialect: %s %v", method, p)
			}
			return map[string]any{"task": map[string]any{"id": "t1", "status": map[string]any{"state": "TASK_STATE_COMPLETED"},
				"artifacts": []any{map[string]any{"parts": []any{map[string]any{"text": "Notes for 2.7.0"}}}}}}
		}
	})
	got, err := dial(t, srv).Chat(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, ChatOptions{})
	if err != nil || got != "Notes for 2.7.0" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestA2AFailuresQuestionsAndErrorsAreSaidPlainly(t *testing.T) {
	for _, c := range []struct {
		name   string
		result any
		want   string
		isErr  bool
	}{
		{"failed", map[string]any{"kind": "task", "id": "t", "status": map[string]any{"state": "failed", "message": map[string]any{"parts": []any{map[string]any{"text": "no changelog"}}}}}, "failed the task: no changelog", true},
		{"question", map[string]any{"kind": "task", "id": "t", "status": map[string]any{"state": "input-required", "message": map[string]any{"parts": []any{map[string]any{"text": "Which version?"}}}}}, "Which version?", false},
	} {
		srv := a2aAgent(t, "0.3.0", func(string, map[string]any, int32) any { return c.result })
		got, err := dial(t, srv).Chat(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, ChatOptions{})
		if c.isErr && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: want error %q, got %q %v", c.name, c.want, got, err)
		}
		if !c.isErr && got != c.want {
			t.Errorf("%s: want %q, got %q %v", c.name, c.want, got, err)
		}
	}
	rpcErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"Method not found"}}`))
	}))
	defer rpcErr.Close()
	_, err := NewA2AProvider(rpcErr.URL, "", "", "c", "0.3").Chat(context.Background(), []ChatMessage{{Role: "user", Content: "x"}}, ChatOptions{})
	if err == nil || !strings.Contains(err.Error(), "Method not found") {
		t.Fatalf("want the remote's JSON-RPC error, got %v", err)
	}
}

func TestA2ACardRules(t *testing.T) {
	if got := cardCandidates("https://agents.acme.com/notes"); got[0] != "https://agents.acme.com/notes/.well-known/agent-card.json" || len(got) != 4 {
		t.Errorf("candidates from an agent address: %v", got)
	}
	if got := cardCandidates("https://acme.com/cards/notes.json"); len(got) != 1 {
		t.Errorf("a card address is used as given: %v", got)
	}
	restOnly := rawCard{Name: "x", URL: "https://a/x", PreferredTransport: "HTTP+JSON"}
	if _, _, err := restOnly.jsonRPCEndpoint(); err == nil || !strings.Contains(err.Error(), "JSON-RPC") {
		t.Errorf("an agent without JSON-RPC must be refused with the reason: %v", err)
	}
	listed := rawCard{Name: "x", URL: "https://a/rest", PreferredTransport: "HTTP+JSON", ProtocolVersion: "1.0",
		AdditionalInterfaces: []rawInterface{{URL: "https://a/rpc", Transport: "JSONRPC"}}}
	if u, v, err := listed.jsonRPCEndpoint(); err != nil || u != "https://a/rpc" || v != "1.0" {
		t.Errorf("a listed JSON-RPC interface is found: %s %s %v", u, v, err)
	}
	for v, want := range map[string]bool{"1.0": true, "v1.2": true, "0.3.0": false, "": false} {
		if isV1(v) != want {
			t.Errorf("isV1(%q) != %v", v, want)
		}
	}
	if a2aState("TASK_STATE_INPUT_REQUIRED") != "input-required" || a2aState("completed") != "completed" {
		t.Error("task states are not normalised")
	}
}

func TestA2APromptCarriesTheRecentConversationWithoutSystemText(t *testing.T) {
	p := a2aPrompt([]ChatMessage{{Role: "system", Content: "SECRET RULES"}, {Role: "user", Content: "Q1"}, {Role: "assistant", Content: "A1"}, {Role: "user", Content: "Q2"}})
	if strings.Contains(p, "SECRET") || !strings.Contains(p, "Person: Q1") || !strings.Contains(p, "You: A1") || !strings.HasSuffix(p, "Person: Q2") {
		t.Fatalf("prompt: %q", p)
	}
	if a2aPrompt([]ChatMessage{{Role: "user", Content: "just this"}}) != "just this" {
		t.Fatal("a single request is sent as it is")
	}
}
