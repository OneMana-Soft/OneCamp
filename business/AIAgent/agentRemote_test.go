package business

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// The endpoint goes through the same front door as a custom model endpoint,
// and a header name has to be one.
func TestARemoteBrainIsValidatedLikeAnyEndpoint(t *testing.T) {
	if ep, hdr, err := validateRemoteBrain("  ", "x-token", "s"); err != nil || ep != "" || hdr != "" {
		t.Errorf("no endpoint must mean no remote and no header, got %q %q %v", ep, hdr, err)
	}
	ep, hdr, err := validateRemoteBrain("https://bots.example.com/ag-ui/", "X-OpenBot-Agent-Token", "tok")
	if err != nil || ep != "https://bots.example.com/ag-ui" || hdr != "X-OpenBot-Agent-Token" {
		t.Errorf("good input: %q %q %v", ep, hdr, err)
	}
	for _, bad := range []struct{ ep, hdr, secret string }{
		{"ftp://bots.example.com", "", ""},
		{"https://user:pw@bots.example.com", "", ""},
		{"https://bots.example.com", "X-Token: injected", ""},
		{"https://bots.example.com", "has space", ""},
		{"https://bots.example.com", "", strings.Repeat("x", maxAGUIAuthSecretLen+1)},
	} {
		if _, _, err := validateRemoteBrain(bad.ep, bad.hdr, bad.secret); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

// The client never sees a stored secret, so an edit that did not type a new
// one keeps it; clearing the endpoint clears the credential with it.
func TestTheStoredSecretSurvivesAnEditThatDidNotTouchIt(t *testing.T) {
	if got := remoteSecretToStore("https://r", "", "old"); got != "old" {
		t.Errorf("empty input must keep the stored secret, got %q", got)
	}
	if got := remoteSecretToStore("https://r", " new ", "old"); got != "new" {
		t.Errorf("a new secret must replace the old, got %q", got)
	}
	if got := remoteSecretToStore("", "new", "old"); got != "" {
		t.Errorf("no endpoint must store no secret, got %q", got)
	}
}

// An agent whose secret cannot be read does not run: the run fails with the
// reason instead of going out with an empty credential.
func TestAnUnreadableSecretStopsTheRun(t *testing.T) {
	a := &model.AiAgent{Id: uuid.New(), AGUIEndpoint: "https://r", AGUIAuthUnreadable: true}
	if _, err := remoteProvider(a, "run"); err == nil || !strings.Contains(err.Error(), "AI_CONFIG_KEK") {
		t.Errorf("want the KEK explanation, got %v", err)
	}
	a.AGUIAuthUnreadable = false
	p, err := remoteProvider(a, "run")
	if err != nil || p == nil || p.Label() != "agui:r" {
		t.Errorf("a readable agent must get a provider: %v %v", p, err)
	}
}

type fakeRemoteReporter struct {
	ai.LLMProvider
	events []ai.RemoteToolEvent
}

func (f *fakeRemoteReporter) TakeRemoteToolEvents() []ai.RemoteToolEvent {
	ev := f.events
	f.events = nil
	return ev
}

type plainProvider struct{ ai.LLMProvider }

// What the remote did on its own is written into the transcript marked as
// its own, and a provider that is not a remote brain adds nothing.
func TestRemoteWorkIsRecordedAsRemote(t *testing.T) {
	rr := &fakeRemoteReporter{events: []ai.RemoteToolEvent{{Name: "browser_open", Arguments: `{"url":"x"}`, Result: "opened"}, {Name: " ", Result: "?"}}}
	recs := remoteWorkRecords(rr)
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if !recs[0].Remote || recs[0].Tool != "browser_open" || recs[0].Result != "opened" || recs[0].Params["arguments"] != `{"url":"x"}` {
		t.Errorf("first record = %+v", recs[0])
	}
	if recs[1].Tool != "(unnamed)" {
		t.Errorf("an unnamed remote call must still be recorded: %+v", recs[1])
	}
	if again := remoteWorkRecords(rr); again != nil {
		t.Errorf("events must be drained: %+v", again)
	}
	if got := remoteWorkRecords(&plainProvider{}); got != nil {
		t.Errorf("a plain provider must add nothing, got %+v", got)
	}
}

// The activity feed names the remote's tools among what ran and leaves them
// out of the count of actions this workspace took.
func TestRemoteWorkIsNotCountedAsAWorkspaceAction(t *testing.T) {
	steps := `[{"tool_calls":[{"tool":"browser_open","result":"opened","remote":true},{"tool":"send_message","result":"sent"}]}]`
	tools, actions := parseRunTranscript(steps)
	if len(tools) != 2 || tools[0] != "browser_open" || tools[1] != "send_message" {
		t.Errorf("tools = %v", tools)
	}
	if actions != 1 {
		t.Errorf("actions = %d, want only the one this workspace ran", actions)
	}
}

// The run's audit row says whose reasoning it was, by host only.
func TestTheAuditRowNamesTheRemoteBrainByHost(t *testing.T) {
	a := &model.AiAgent{Id: uuid.New(), Name: "scout", AGUIEndpoint: "https://bots.example.com/ag-ui/secret-path", AGUIAuthSecret: "tok"}
	meta := runAuditMeta(finishedRun{Agent: a, RunID: uuid.New(), Status: model.RunSucceeded})
	if got, _ := meta["remote_brain"].(string); got != "agui:bots.example.com" {
		t.Errorf("remote_brain = %q", got)
	}
	for k, v := range meta {
		if s, ok := v.(string); ok && (strings.Contains(s, "secret-path") || strings.Contains(s, "tok")) {
			t.Errorf("%s leaked the path or the secret: %q", k, s)
		}
	}
	local := &model.AiAgent{Id: uuid.New(), Name: "local"}
	if _, has := runAuditMeta(finishedRun{Agent: local, RunID: uuid.New(), Status: model.RunSucceeded})["remote_brain"]; has {
		t.Error("a local agent must not carry remote_brain")
	}
}

// Two facts about a remote brain that only RunAgent can hold, pinned at the
// source because RunAgent needs a database to run: it always takes the native
// tool path (AG-UI carries calls as structure), and it never falls back to a
// workspace model (that would quietly turn somebody else's agent into ours).
func TestARemoteBrainIsNativeAndNeverFallsBack(t *testing.T) {
	src, err := os.ReadFile("agentRunner.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "(remote || (agentNativeToolsEnabled() && len(enabledTools) > 0))") {
		t.Error("the native path is no longer forced for a remote brain")
	}
	if !strings.Contains(body, "if len(toolSpecs) == 0 && !remote {") {
		t.Error("a remote brain with no tools is dropped to the text path, which it cannot speak")
	}
	fallback := strings.Index(body, "fallbackLLM, fallbackCB = svc.ResolveFallbackModel(loopCtx)")
	guard := strings.LastIndex(body[:max(fallback, 0)], "if !remote {")
	if fallback < 0 || guard < 0 || fallback-guard > 300 {
		t.Errorf("the fallback model is resolved for a remote brain too (guard=%d resolve=%d)", guard, fallback)
	}
}

// The check runs the real provider against a real endpoint, so what it proves
// is what a run would do: the same URL rules, the same header, the same
// credential. A transport failure comes back as an answer (ok:false with the
// reason), not as an error, because "the remote answered 401" is the thing the
// admin asked.
func TestCheckingARemoteEndpointReportsWhatCameBack(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-token")
		if gotHeader != "s3cret" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"a\",\"delta\":\"I received it.\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"TOOL_CALL_START\",\"toolCallId\":\"c1\",\"toolCallName\":\"send_message\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"TOOL_CALL_END\",\"toolCallId\":\"c1\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"RUN_FINISHED\"}\n\n")
	}))
	defer srv.Close()

	res, err := CheckRemoteBrain(context.Background(), RemoteBrainCheck{
		Endpoint: srv.URL, AuthHeader: "x-token", AuthSecret: "s3cret",
	}, Actor{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !res.OK || res.Reply != "I received it." {
		t.Errorf("result = %+v", res)
	}
	if len(res.ToolsAsked) != 1 || res.ToolsAsked[0] != "send_message" {
		t.Errorf("tools asked = %v", res.ToolsAsked)
	}

	bad, err := CheckRemoteBrain(context.Background(), RemoteBrainCheck{
		Endpoint: srv.URL, AuthHeader: "x-token", AuthSecret: "wrong",
	}, Actor{})
	if err != nil {
		t.Fatalf("a refused check must be an answer, not an error: %v", err)
	}
	if bad.OK || !strings.Contains(bad.Error, "401") {
		t.Errorf("refused result = %+v", bad)
	}
	if strings.Contains(bad.Error, "wrong") || strings.Contains(bad.Error, "s3cret") {
		t.Errorf("the secret reached the message: %q", bad.Error)
	}
}

// An endpoint this workspace will not dial at all is refused before any
// request, with the reason an admin can act on.
func TestCheckingRefusesAnEndpointItWouldNeverDial(t *testing.T) {
	for _, bad := range []RemoteBrainCheck{
		{Endpoint: ""},
		{Endpoint: "ftp://bots.example.com"},
		{Endpoint: "https://bots.example.com", AuthHeader: "X-Token: injected"},
	} {
		if _, err := CheckRemoteBrain(context.Background(), bad, Actor{}); err == nil {
			t.Errorf("%+v must be refused before anything is dialled", bad)
		}
	}
}

// The reason is translated when it is ours rather than the remote's: a dial
// the guard refused says which rule refused it.
func TestACheckSaysWhenTheRefusalWasOurs(t *testing.T) {
	if got := remoteCheckError(errors.New("agui: ai: local-only mode is on — refusing to send content to a non-local model endpoint: 1.2.3.4")); !strings.HasPrefix(got, "Local-only AI mode is on") {
		t.Errorf("local-only = %q", got)
	}
	if got := remoteCheckError(errors.New("agui: ai: connection to this address is blocked for security (cloud metadata / link-local)")); !strings.Contains(got, "cloud metadata") {
		t.Errorf("blocked = %q", got)
	}
	if got := remoteCheckError(errors.New("agui: remote answered 500: boom")); got != "remote answered 500: boom" {
		t.Errorf("an ordinary failure must be passed through: %q", got)
	}
}

// Go wraps a dial failure as `Post "<url>": <reason>`. The admin typed that
// url and is looking at it; the reason is the part they do not have.
func TestACheckLeadsWithTheReasonNotTheUrl(t *testing.T) {
	// The REAL shape, copied from a live refusal: this package wraps, Go's HTTP
	// client wraps, and the dialer wrapped. My first version of this test
	// invented a one-layer string, passed, and shipped a trim that never fired.
	got := remoteCheckError(errors.New(`agui: Post "http://example.com/ag-ui": agui: refusing to send workspace content to a remote agent outside your own network over plain http; use https (example.com resolves to 93.184.216.34)`))
	if !strings.HasPrefix(got, "refusing to send workspace content") {
		t.Errorf("message = %q", got)
	}
	// An error that is not wrapped that way is left alone.
	if got := remoteCheckError(errors.New("agui: remote answered 500: boom")); got != "remote answered 500: boom" {
		t.Errorf("unwrapped message = %q", got)
	}
	// And a url containing the marker inside it does not confuse the trim.
	if got := remoteCheckError(errors.New(`Post "https://x.example.com/a": dial tcp: lookup failed`)); got != "dial tcp: lookup failed" {
		t.Errorf("message = %q", got)
	}
}
