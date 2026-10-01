package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// sse writes one AG-UI event as a text/event-stream frame.
func sse(w io.Writer, ev map[string]interface{}) {
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// aguiFixture is a remote that records what it was sent and replies with a
// scripted stream.
type aguiFixture struct {
	t      *testing.T
	seen   []aguiRunInput
	header http.Header
	script func(w io.Writer, in aguiRunInput)
}

func (f *aguiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.header = r.Header.Clone()
	var in aguiRunInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.t.Errorf("remote could not decode the run input: %v", err)
		http.Error(w, "bad input", 400)
		return
	}
	f.seen = append(f.seen, in)
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, map[string]interface{}{"type": "RUN_STARTED", "threadId": in.ThreadID, "runId": in.RunID})
	f.script(w, in)
}

func newRemote(t *testing.T, script func(w io.Writer, in aguiRunInput)) (*aguiFixture, *httptest.Server) {
	t.Helper()
	f := &aguiFixture{t: t, script: script}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func finish(w io.Writer, in aguiRunInput) {
	sse(w, map[string]interface{}{"type": "RUN_FINISHED", "threadId": in.ThreadID, "runId": in.RunID})
}

// The remote sees our conversation and our tools, and what it asks for comes
// back as ordinary tool calls the runner will govern like any other.
func TestAGUIRunCarriesOurToolsAndReturnsTheCalls(t *testing.T) {
	f, srv := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_START", "messageId": "a", "role": "assistant"})
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "Looking that "})
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "up."})
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_END", "messageId": "a"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_START", "toolCallId": "c1", "toolCallName": "web_search"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_ARGS", "toolCallId": "c1", "delta": `{"query":`})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_ARGS", "toolCallId": "c1", "delta": `"onecamp"}`})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_END", "toolCallId": "c1"})
		finish(w, in)
	})
	p := NewAGUIProvider(srv.URL+"/ag-ui", "", "s3cret", "run-1")

	msgs := []ChatMessage{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "What is OneCamp?"},
	}
	tools := []ToolSpec{{Name: "web_search", Description: "search", Parameters: map[string]interface{}{"type": "object"}}}
	text, calls, err := p.ChatWithTools(context.Background(), msgs, tools, ChatOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if text != "Looking that up." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0].ID != "c1" || calls[0].Name != "web_search" || calls[0].Arguments != `{"query":"onecamp"}` {
		t.Errorf("calls = %+v", calls)
	}

	in := f.seen[0]
	if in.ThreadID != "run-1" || in.RunID == "" {
		t.Errorf("thread/run = %q/%q", in.ThreadID, in.RunID)
	}
	if len(in.Tools) != 1 || in.Tools[0].Name != "web_search" {
		t.Errorf("the remote was not offered our tool: %+v", in.Tools)
	}
	if len(in.Messages) != 2 || in.Messages[0].Role != "system" || in.Messages[1].Role != "user" || in.Messages[1].Content != "What is OneCamp?" {
		t.Errorf("messages = %+v", in.Messages)
	}
	if got := f.header.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want a bearer token", got)
	}
	if got := f.header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
}

// A tool result goes back as a tool-role message keyed by the call it
// answers, and the assistant turn that made the call goes back with it, so a
// remote sees the same pairing a model provider would.
func TestAGUIToolResultsGoBackKeyedByCall(t *testing.T) {
	f, srv := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "Done."})
		finish(w, in)
	})
	p := NewAGUIProvider(srv.URL, "", "", "t")
	msgs := []ChatMessage{
		{Role: "user", Content: "search"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "web_search", Arguments: ""}}},
		{Role: "tool", Content: "3 results", ToolCallID: "c1", Name: "web_search"},
	}
	if _, _, err := p.ChatWithTools(context.Background(), msgs, nil, ChatOptions{}); err != nil {
		t.Fatal(err)
	}
	in := f.seen[0]
	if len(in.Messages) != 3 {
		t.Fatalf("messages = %+v", in.Messages)
	}
	a := in.Messages[1]
	if a.Role != "assistant" || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "c1" || a.ToolCalls[0].Type != "function" ||
		a.ToolCalls[0].Function.Name != "web_search" || a.ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("assistant turn = %+v", a)
	}
	r := in.Messages[2]
	if r.Role != "tool" || r.ToolCallID != "c1" || r.Content != "3 results" {
		t.Errorf("tool turn = %+v", r)
	}
	if got := f.header.Get("Authorization"); got != "" {
		t.Errorf("no secret configured, but Authorization = %q", got)
	}
}

// A call the remote answered itself is its own work on its own machine: it
// is reported to the runner, never returned as something for us to execute.
func TestAGUIAToolTheRemoteRanItselfIsReportedNotReturned(t *testing.T) {
	_, srv := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "TOOL_CALL_START", "toolCallId": "b1", "toolCallName": "browser_open"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_ARGS", "toolCallId": "b1", "delta": `{"url":"https://example.com"}`})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_END", "toolCallId": "b1"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_RESULT", "toolCallId": "b1", "messageId": "r1", "role": "tool", "content": "opened"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_CHUNK", "toolCallId": "c2", "toolCallName": "send_message", "delta": `{"channel":"general"}`})
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "Posted."})
		finish(w, in)
	})
	p := NewAGUIProvider(srv.URL, "", "", "t")
	text, calls, err := p.ChatWithTools(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, nil, ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Posted." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0].Name != "send_message" || calls[0].Arguments != `{"channel":"general"}` {
		t.Errorf("calls for us = %+v, want only send_message", calls)
	}
	remote := p.TakeRemoteToolEvents()
	if len(remote) != 1 || remote[0].Name != "browser_open" || remote[0].Result != "opened" || !strings.Contains(remote[0].Arguments, "example.com") {
		t.Errorf("remote work = %+v", remote)
	}
	if again := p.TakeRemoteToolEvents(); len(again) != 0 {
		t.Errorf("remote events were not cleared on take: %+v", again)
	}
}

// A run that stops without finishing, or that says it failed, is an error the
// runner can show, never a silent empty answer.
func TestAGUIAStreamThatStopsOrFailsIsAnError(t *testing.T) {
	_, quiet := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "half"})
	})
	if _, _, err := NewAGUIProvider(quiet.URL, "", "", "t").ChatWithTools(context.Background(), nil, nil, ChatOptions{}); err == nil || !strings.Contains(err.Error(), "without finishing") {
		t.Errorf("a stream that started and stopped must fail, got %v", err)
	}

	_, failing := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "RUN_ERROR", "message": "model quota exhausted", "code": "quota"})
	})
	if _, _, err := NewAGUIProvider(failing.URL, "", "", "t").ChatWithTools(context.Background(), nil, nil, ChatOptions{}); err == nil || !strings.Contains(err.Error(), "model quota exhausted (quota)") {
		t.Errorf("RUN_ERROR must surface its message, got %v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Unauthorized."}`, http.StatusUnauthorized)
	}))
	t.Cleanup(denied.Close)
	if _, _, err := NewAGUIProvider(denied.URL, "", "wrong", "t").ChatWithTools(context.Background(), nil, nil, ChatOptions{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused request must name the status, got %v", err)
	}
}

// The credential goes where the admin said: a named header carries the raw
// secret, which is how the OpenBot bots and similar peers authenticate.
func TestAGUINamedHeaderCarriesTheSecret(t *testing.T) {
	f, srv := newRemote(t, func(w io.Writer, in aguiRunInput) { finish(w, in) })
	p := NewAGUIProvider(srv.URL, "x-openbot-agent-token", "tok", "t")
	if _, _, err := p.ChatWithTools(context.Background(), nil, nil, ChatOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.header.Get("x-openbot-agent-token"); got != "tok" {
		t.Errorf("named header = %q", got)
	}
	if got := f.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization must be untouched when a named header is used, got %q", got)
	}
}

// State the remote publishes comes back to it on the next run, which is how
// AG-UI carries an agent's memory between runs.
func TestAGUIStateRoundTrips(t *testing.T) {
	f, srv := newRemote(t, func(w io.Writer, in aguiRunInput) {
		sse(w, map[string]interface{}{"type": "STATE_SNAPSHOT", "snapshot": map[string]interface{}{"step": 2}})
		finish(w, in)
	})
	p := NewAGUIProvider(srv.URL, "", "", "t")
	for i := 0; i < 2; i++ {
		if _, _, err := p.ChatWithTools(context.Background(), nil, nil, ChatOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(f.seen[0].State); got != "{}" {
		t.Errorf("first run state = %s, want empty object", got)
	}
	if got := string(f.seen[1].State); got != `{"step":2}` {
		t.Errorf("second run state = %s, want the snapshot", got)
	}
}

// Chat with no tools on offer returns prose; a remote that asks for a tool
// anyway gets that fact into the text rather than a call nothing will run.
func TestAGUIChatWithoutToolsNeverReturnsACall(t *testing.T) {
	_, srv := newRemote(t, func(w io.Writer, in aguiRunInput) {
		if len(in.Tools) != 0 {
			fmt.Fprint(w, "")
		}
		sse(w, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "Sure."})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_START", "toolCallId": "c1", "toolCallName": "send_dm"})
		sse(w, map[string]interface{}{"type": "TOOL_CALL_END", "toolCallId": "c1"})
		finish(w, in)
	})
	got, err := NewAGUIProvider(srv.URL, "", "", "t").Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, ChatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Sure.") || !strings.Contains(got, "send_dm") || !strings.Contains(got, "no tools were offered") {
		t.Errorf("got %q", got)
	}
}

// The label a run records names the protocol and the host, never the path
// (which may carry a routing token) and never the secret.
func TestAGUILabelNamesOnlyTheHost(t *testing.T) {
	if got := AGUILabel("https://bots.example.com:4200/ag-ui/abc123"); got != "agui:bots.example.com:4200" {
		t.Errorf("label = %q", got)
	}
	if got := AGUILabel("not a url"); got != "agui" {
		t.Errorf("unparseable label = %q", got)
	}
}

// The SSE reader joins multi-line data and skips comments and other fields.
func TestSSEReaderJoinsDataLinesAndSkipsTheRest(t *testing.T) {
	stream := ": keepalive\nevent: message\nid: 1\ndata: {\"type\":\"TEXT_MESSAGE_CONTENT\",\ndata: \"delta\":\"ab\"}\n\ndata: {\"type\":\"RUN_FINISHED\"}\n\n"
	run, err := readAGUIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Text != "ab" || len(run.Calls) != 0 {
		t.Errorf("text=%q calls=%v", run.Text, run.Calls)
	}
}

// A remote must not be able to push an unbounded number of calls into one
// run: the runner's own ceiling on parallel calls applies here too.
func TestAGUIBoundsToolCallsPerRun(t *testing.T) {
	var sb strings.Builder
	for i := 0; i <= aguiMaxToolCalls; i++ {
		sse(&sb, map[string]interface{}{"type": "TOOL_CALL_START", "toolCallId": fmt.Sprintf("c%d", i), "toolCallName": "web_search"})
	}
	sse(&sb, map[string]interface{}{"type": "RUN_FINISHED"})
	if _, err := readAGUIStream(strings.NewReader(sb.String()), nil, nil); err == nil || !strings.Contains(err.Error(), "too many tool calls") {
		t.Errorf("want a bound error, got %v", err)
	}
}

// The remote's own scratchpad is kept by patch as well as by snapshot. The
// frameworks people build with send a snapshot once and patches after it, so a
// client that reads only snapshots freezes that state at the first one and
// hands the agent back something it never wrote.
func TestAGUIStateIsKeptByPatchAsWellAsSnapshot(t *testing.T) {
	var sb strings.Builder
	sse(&sb, map[string]interface{}{"type": "STATE_SNAPSHOT", "snapshot": map[string]interface{}{"step": 1, "notes": []string{"a"}}})
	sse(&sb, map[string]interface{}{"type": "STATE_DELTA", "delta": []map[string]interface{}{
		{"op": "replace", "path": "/step", "value": 2},
		{"op": "add", "path": "/notes/-", "value": "b"},
	}})
	sse(&sb, map[string]interface{}{"type": "RUN_FINISHED"})

	run, err := readAGUIStream(strings.NewReader(sb.String()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.StateErr != nil {
		t.Fatalf("patch did not apply: %v", run.StateErr)
	}
	if got := string(run.State); got != `{"notes":["a","b"],"step":2}` {
		t.Errorf("state = %s", got)
	}

	// A patch arriving with no snapshot first applies to what the run started
	// with, which is the state the last run left.
	var later strings.Builder
	sse(&later, map[string]interface{}{"type": "STATE_DELTA", "delta": []map[string]interface{}{{"op": "replace", "path": "/step", "value": 3}}})
	sse(&later, map[string]interface{}{"type": "RUN_FINISHED"})
	run, err = readAGUIStream(strings.NewReader(later.String()), json.RawMessage(`{"step":2}`), nil)
	if err != nil || run.StateErr != nil {
		t.Fatalf("err=%v stateErr=%v", err, run.StateErr)
	}
	if got := string(run.State); got != `{"step":3}` {
		t.Errorf("state = %s", got)
	}
}

// A patch that does not apply leaves NO state rather than a stale one: handing
// an agent a document it never wrote is worse than handing it none. The run
// itself still completes, because this is the remote's scratchpad and not the
// workspace's record.
func TestAGUIAnUnapplyablePatchDropsTheStateAndNotTheRun(t *testing.T) {
	var sb strings.Builder
	sse(&sb, map[string]interface{}{"type": "STATE_DELTA", "delta": []map[string]interface{}{{"op": "replace", "path": "/missing", "value": 1}}})
	sse(&sb, map[string]interface{}{"type": "STATE_DELTA", "delta": []map[string]interface{}{{"op": "add", "path": "/ok", "value": 1}}})
	sse(&sb, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "still answered"})
	sse(&sb, map[string]interface{}{"type": "RUN_FINISHED"})

	run, err := readAGUIStream(strings.NewReader(sb.String()), json.RawMessage(`{"step":1}`), nil)
	if err != nil {
		t.Fatalf("a bad patch must not fail the run: %v", err)
	}
	if run.StateErr == nil {
		t.Fatal("the failure was not reported")
	}
	if run.State != nil {
		t.Errorf("stale state was kept: %s", run.State)
	}
	if run.Text != "still answered" {
		t.Errorf("text = %q", run.Text)
	}

	// A snapshot re-bases: the remote has just said everything, so an earlier
	// failed patch no longer matters.
	var recovered strings.Builder
	sse(&recovered, map[string]interface{}{"type": "STATE_DELTA", "delta": []map[string]interface{}{{"op": "replace", "path": "/missing", "value": 1}}})
	sse(&recovered, map[string]interface{}{"type": "STATE_SNAPSHOT", "snapshot": map[string]interface{}{"fresh": true}})
	sse(&recovered, map[string]interface{}{"type": "RUN_FINISHED"})
	run, _ = readAGUIStream(strings.NewReader(recovered.String()), nil, nil)
	if run.StateErr != nil || string(run.State) != `{"fresh":true}` {
		t.Errorf("a snapshot must clear the failure: state=%s err=%v", run.State, run.StateErr)
	}
}

// A remote may carry its answer on RUN_FINISHED rather than streaming it.
// Returning nothing there is the silent-empty-answer failure this client
// exists to avoid.
func TestAGUIAnAnswerOnRunFinishedIsStillAnAnswer(t *testing.T) {
	var sb strings.Builder
	sse(&sb, map[string]interface{}{"type": "RUN_FINISHED", "result": "the answer"})
	run, err := readAGUIStream(strings.NewReader(sb.String()), nil, nil)
	if err != nil || run.Text != "the answer" {
		t.Fatalf("text = %q, err = %v", run.Text, err)
	}

	// An object result is carried as it was written, for a remote that answers
	// with structure.
	var obj strings.Builder
	sse(&obj, map[string]interface{}{"type": "RUN_FINISHED", "result": map[string]interface{}{"ok": true}})
	run, _ = readAGUIStream(strings.NewReader(obj.String()), nil, nil)
	if run.Text != `{"ok":true}` {
		t.Errorf("object result = %q", run.Text)
	}

	// Streamed text wins, so a remote that does both is not made to repeat.
	var both strings.Builder
	sse(&both, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "streamed"})
	sse(&both, map[string]interface{}{"type": "RUN_FINISHED", "result": "duplicate"})
	run, _ = readAGUIStream(strings.NewReader(both.String()), nil, nil)
	if run.Text != "streamed" {
		t.Errorf("text = %q, want the streamed answer", run.Text)
	}

	// A null result is not an answer.
	var empty strings.Builder
	sse(&empty, map[string]interface{}{"type": "RUN_FINISHED", "result": nil})
	run, _ = readAGUIStream(strings.NewReader(empty.String()), nil, nil)
	if run.Text != "" {
		t.Errorf("null result became %q", run.Text)
	}
}

// One field name carries two types: a text delta is a string, a state delta is
// a patch array. A delta of the wrong shape contributes nothing rather than
// putting its own JSON into somebody's message.
func TestAGUIADeltaOfTheWrongShapeAddsNothing(t *testing.T) {
	var sb strings.Builder
	sse(&sb, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": map[string]interface{}{"not": "text"}})
	sse(&sb, map[string]interface{}{"type": "TEXT_MESSAGE_CONTENT", "messageId": "a", "delta": "real"})
	sse(&sb, map[string]interface{}{"type": "RUN_FINISHED"})
	run, err := readAGUIStream(strings.NewReader(sb.String()), nil, nil)
	if err != nil || run.Text != "real" {
		t.Fatalf("text = %q, err = %v", run.Text, err)
	}
}

// A run does not send a question. It sends the agent's instructions, the
// workspace knowledge it was grounded in, the conversation, and the result of
// every tool this workspace ran. That is workspace content leaving the
// building on every step, so an endpoint outside the customer's own network
// has to be encrypted and has to ask something of its callers.
func TestARemoteOffYourNetworkMustBeHTTPSAndMustHaveASecret(t *testing.T) {
	cases := []struct {
		name, endpoint, secret string
		refused                bool
		says                   string
	}{
		{"https with a secret", "https://bots.example.com/ag-ui", "tok", false, ""},
		{"plain http", "http://bots.example.com/ag-ui", "tok", true, "plain http"},
		{"no secret", "https://bots.example.com/ag-ui", "", true, "no secret"},
		{"neither", "http://bots.example.com/ag-ui", "", true, "plain http and with no secret"},
		{"a blank secret is no secret", "https://bots.example.com/ag-ui", "   ", true, "no secret"},
		{"scheme is read case-insensitively", "HTTPS://bots.example.com/ag-ui", "tok", false, ""},
	}
	for _, c := range cases {
		got := OffLocalRefusal(c.endpoint, c.secret)
		if c.refused && got == "" {
			t.Errorf("%s: allowed off-network, want refused", c.name)
			continue
		}
		if !c.refused && got != "" {
			t.Errorf("%s: refused with %q, want allowed", c.name, got)
			continue
		}
		if c.refused && !strings.Contains(got, c.says) {
			t.Errorf("%s: refusal %q does not say %q", c.name, got, c.says)
		}
	}
}

// And it is the ADDRESS that decides, not the URL: the same endpoint is fine
// on your own network, which is where a bot in the same compose file lives.
func TestTheSameRemoteIsFineOnYourOwnNetwork(t *testing.T) {
	cfg := httpClientConfig{guardSSRF: true, offLocalRefusal: OffLocalRefusal("http://agent-bot:4200/ag-ui", "")}
	if cfg.offLocalRefusal == "" {
		t.Fatal("fixture should be one this rule refuses off-network")
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.9", "172.20.0.4", "::1"} {
		if err := dialAllowed(cfg, false, "agent-bot", net.ParseIP(ip)); err != nil {
			t.Errorf("%s is on the customer's own network and was refused: %v", ip, err)
		}
	}
	for _, ip := range []string{"8.8.8.8", "93.184.216.34", "2606:4700::1111"} {
		err := dialAllowed(cfg, false, "bots.example.com", net.ParseIP(ip))
		if err == nil {
			t.Errorf("%s is off-network and was allowed", ip)
			continue
		}
		if !strings.Contains(err.Error(), ip) || !strings.Contains(err.Error(), "bots.example.com") {
			t.Errorf("the refusal must name the host and the address it resolved to: %v", err)
		}
	}
}

// The three egress rules in the order that matters. Local-only mode outranks a
// per-client permission, and a metadata address is refused whatever else is
// true, because those are the two an operator cannot opt out of per endpoint.
func TestTheEgressRulesApplyInTheRightOrder(t *testing.T) {
	public := net.ParseIP("8.8.8.8")
	metadata := net.ParseIP("169.254.169.254")

	// Local-only beats an endpoint that would otherwise be allowed.
	if err := dialAllowed(httpClientConfig{}, true, "api.openai.com", public); err == nil ||
		!strings.Contains(err.Error(), "local-only") {
		t.Errorf("local-only mode did not refuse a public address: %v", err)
	}
	// And it is checked before the per-endpoint rule, so the operator-wide
	// switch is the reason given.
	both := httpClientConfig{guardSSRF: true, offLocalRefusal: "per-endpoint reason"}
	if err := dialAllowed(both, true, "bots.example.com", public); err == nil ||
		!strings.Contains(err.Error(), "local-only") {
		t.Errorf("want the local-only reason first, got %v", err)
	}
	// Metadata is refused for any guarded client.
	if err := dialAllowed(httpClientConfig{guardSSRF: true}, false, "x", metadata); err == nil ||
		!strings.Contains(err.Error(), "blocked for security") {
		t.Errorf("metadata address was not refused: %v", err)
	}
	// An unguarded client with nothing to say about locality dials anything.
	if err := dialAllowed(httpClientConfig{}, false, "api.openai.com", public); err != nil {
		t.Errorf("a trusted built-in must still be dialable: %v", err)
	}
}

// The rule only protects anybody if the provider installs it, and that is one
// line handing a value to a client whose config nothing outside can read.
// Pinned at the source, the same way this codebase guards its other one-line
// couplings, because a test that cannot see the wire cannot report it cut.
func TestTheProviderInstallsTheOffNetworkRule(t *testing.T) {
	src, err := os.ReadFile("agui.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func NewAGUIProvider(")
	if start < 0 {
		t.Fatal("NewAGUIProvider is gone")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("cannot find the end of NewAGUIProvider")
	}
	ctor := body[start : start+end]
	if !strings.Contains(ctor, "offLocalRefusal: OffLocalRefusal(endpoint, secret)") {
		t.Error("the provider's HTTP client no longer carries the off-network refusal, so a " +
			"remote agent could be sent workspace content over plain http or with no credential")
	}
	if !strings.Contains(ctor, "guardSSRF:       true") && !strings.Contains(ctor, "guardSSRF: true") {
		t.Error("the provider's HTTP client no longer has the SSRF guard")
	}
}

// An answered request carrying NO events is a different fault from one that
// stopped partway, and it has a different fix: something between here and the
// agent swallowed the stream. Reported as the same sentence, it is
// undebuggable, because from this side a healthy silent agent and a proxy that
// ate everything look identical.
func TestAStreamThatNeverStartedIsNotAStreamThatStopped(t *testing.T) {
	// A raw handler, not the fixture: the fixture always opens with RUN_STARTED,
	// and what is being reproduced here is a stream where literally nothing
	// arrives, which is what a proxy that does not carry SSE produces.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(silent.Close)
	_, _, err := NewAGUIProvider(silent.URL, "", "", "t").ChatWithTools(context.Background(), nil, nil, ChatOptions{})
	if err == nil {
		t.Fatal("a 200 with no events must not read as a successful run")
	}
	if !errors.Is(err, errNoEventsArrived) {
		t.Errorf("want the no-events fault, got %v", err)
	}
	if !strings.Contains(err.Error(), "buffering or stripping") {
		t.Errorf("the error does not name the usual cause: %v", err)
	}
	if strings.Contains(err.Error(), "without finishing") {
		t.Error("a stream that never started must not be reported as one that stopped")
	}
}

// Cloudflare quick tunnels are the fastest way to put a laptop agent behind
// https, which is exactly what the off-network rule asks for, and they do not
// carry server-sent events. So the obvious first attempt at this feature fails
// in the one way that leaves no trace, and the error says so by name.
func TestAQuickTunnelIsNamedAsTheCause(t *testing.T) {
	hint := streamSwallowedHint("https://random-words-here.trycloudflare.com/ag-ui")
	if !strings.Contains(hint, "trycloudflare.com") || !strings.Contains(hint, "server-sent events") {
		t.Errorf("hint = %q", hint)
	}
	// Anywhere else gets the general diagnosis, not a guess about Cloudflare.
	if got := streamSwallowedHint("https://bots.example.com/ag-ui"); strings.Contains(got, "trycloudflare") {
		t.Errorf("an unrelated host was blamed on Cloudflare: %q", got)
	}

	for _, yes := range []string{
		"https://a-b-c.trycloudflare.com/ag-ui",
		"http://TRYCLOUDFLARE.COM/x",
		"https://trycloudflare.com",
	} {
		if !isQuickTunnel(yes) {
			t.Errorf("%s is a quick tunnel and was not recognised", yes)
		}
	}
	for _, no := range []string{
		"https://bots.example.com/ag-ui",
		// The suffix has to be the domain, not part of a longer name a
		// lookalike could register.
		"https://nottrycloudflare.com/ag-ui",
		"https://trycloudflare.com.evil.example/ag-ui",
		"not a url at all",
		"",
	} {
		if isQuickTunnel(no) {
			t.Errorf("%s is not a quick tunnel and was reported as one", no)
		}
	}
}
