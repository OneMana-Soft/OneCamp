package ai

// A remote agent as a provider.
//
// AG-UI is the wire protocol the current generation of agent frameworks
// speak (LangGraph, CrewAI, Mastra, Pydantic AI, the OpenBot bots, and the
// rest): one POST carrying the conversation and the tools on offer, answered
// with a stream of events that carry text and tool calls back. The half of
// that protocol worth noticing is who owns the tools. Every tool comes from
// the caller. The remote publishes none; it decides which of ours to call and
// ends its run to ask us to run it.
//
// That is exactly the shape of the runner's native tool loop, so a remote
// agent is wired in as a provider rather than as a second loop: the runner
// hands it messages and tool specs, it returns text and tool calls, and each
// call then passes through the same allow-list, scope check, autonomy gate,
// destructive backstop and intent row as a call from any model. Nothing about
// governance is duplicated, because nothing about it is bypassed: a remote
// brain gets a OneCamp body, and the body keeps the rules.
//
// What the remote does on its own machine (a browser it drives, a shell it
// runs) is outside this workspace and outside this record. A tool call it
// answers itself, with TOOL_CALL_RESULT, is reported to the runner as remote
// work and never executed here, so the transcript says what happened without
// claiming to have governed it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// ProviderAGUI is a remote agent reached over the AG-UI protocol.
const ProviderAGUI ProviderType = "agui"

const (
	// aguiMaxResponseBytes bounds one run's event stream. A remote is an
	// admin-configured peer, not an adversary, but it is still a network peer.
	aguiMaxResponseBytes = 8 << 20
	// aguiMaxEvents bounds the number of events one run may emit.
	aguiMaxEvents = 20000
	// aguiMaxToolCalls bounds tool calls per run, matching the ceiling the
	// runner applies to a model's parallel calls.
	aguiMaxToolCalls = 32
	// aguiBreakerFailures / aguiBreakerReset: a dead remote trips fast and is
	// retried after a pause, per endpoint, like any provider.
	aguiBreakerFailures = 3
	aguiBreakerReset    = 2 * time.Minute
)

// RemoteToolEvent is a tool call the remote answered itself within a run.
// It is recorded, never executed, and never governed: the work happened on
// the remote's own machine.
type RemoteToolEvent struct {
	Name      string
	Arguments string
	Result    string
}

// RemoteToolReporter is implemented by a provider that can say what the
// remote did on its own during the last call. The runner records it.
type RemoteToolReporter interface {
	TakeRemoteToolEvents() []RemoteToolEvent
}

// AGUIProvider is one remote agent endpoint. Safe for one run at a time; the
// runner constructs one per run.
type AGUIProvider struct {
	endpoint   string
	authHeader string // "" means Authorization: Bearer
	secret     string
	client     *http.Client
	threadID   string
	breaker    *CircuitBreaker

	mu     sync.Mutex
	state  json.RawMessage // last STATE_SNAPSHOT, round-tripped as AG-UI expects
	remote []RemoteToolEvent
}

// aguiBreakers holds one circuit breaker per endpoint for the process, so a
// remote that is down is not dialled by every run of every agent that uses it.
var aguiBreakers sync.Map

// aguiStateComplaint remembers which endpoints have already been reported for
// sending a state patch this client could not apply.
var aguiStateComplaint sync.Map

func aguiBreakerFor(endpoint string) *CircuitBreaker {
	if cb, ok := aguiBreakers.Load(endpoint); ok {
		return cb.(*CircuitBreaker)
	}
	cb, _ := aguiBreakers.LoadOrStore(endpoint, NewCircuitBreaker(aguiBreakerFailures, aguiBreakerReset))
	return cb.(*CircuitBreaker)
}

// NewAGUIProvider builds a provider for one remote agent. endpoint must have
// passed ValidateHTTPEndpoint; the dial-time guard still applies on top, so a
// remote on a link-local or metadata address is refused when dialled, and
// local-only mode refuses anything off the customer's own network.
//
// threadID is the AG-UI conversation the remote sees; the runner passes the
// run id so each run is its own thread and no remote can confuse two runs'
// histories.
func NewAGUIProvider(endpoint, authHeader, secret, threadID string) *AGUIProvider {
	endpoint = strings.TrimSpace(endpoint)
	return &AGUIProvider{
		endpoint:   endpoint,
		authHeader: strings.TrimSpace(authHeader),
		secret:     secret,
		threadID:   strings.TrimSpace(threadID),
		client: newProviderHTTPClient(httpClientConfig{
			timeout:         0,
			guardSSRF:       true,
			offLocalRefusal: OffLocalRefusal(endpoint, secret),
		}),
		breaker: aguiBreakerFor(endpoint),
	}
}

// OffLocalRefusal says whether this endpoint may be dialled outside the
// customer's own network, and if not, why not.
//
// WHAT IS ACTUALLY BEING SENT decides this. A run does not send a question. It
// sends the agent's instructions, the workspace knowledge it was grounded in,
// the conversation, and the result of every tool this workspace ran for it.
// That is workspace content, leaving the building, on every step.
//
// So two conditions, and both are about that content rather than about the
// remote:
//
//   - PLAIN HTTP is refused off the local network, because the content would
//     cross the internet in the clear and nothing would prove the endpoint that
//     answered is the one that was typed. Inside your own network it is the
//     ordinary case, which is why this is not a blanket rule: a bot in the same
//     compose file is reached at http://agent-bot:4200 and always will be.
//   - NO CREDENTIAL is refused off the local network, because an endpoint that
//     asks nothing of its callers is an endpoint anybody can call, and it is
//     holding a conversation from your workspace. The reference implementations
//     take the same position: OpenBot's own bot refuses to start without a
//     token, and Bedrock's AG-UI contract specifies HTTPS with a bearer token.
//     A bot on your own network is again the exception, and again on purpose.
//
// Empty means no restriction. Enforced at DIAL time against the address
// actually reached, because a hostname cannot be trusted to say where it goes.
func OffLocalRefusal(endpoint, secret string) string {
	insecure := !strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "https://")
	missingSecret := strings.TrimSpace(secret) == ""
	switch {
	case insecure && missingSecret:
		return "agui: refusing to send workspace content to a remote agent outside your own network over plain http and with no secret; use https and set a secret"
	case insecure:
		return "agui: refusing to send workspace content to a remote agent outside your own network over plain http; use https"
	case missingSecret:
		return "agui: refusing to send workspace content to a remote agent outside your own network with no secret; set one so the endpoint is not open to anybody"
	}
	return ""
}

// Breaker is the endpoint's circuit breaker, for the runner to record results
// against as it does for any provider.
func (p *AGUIProvider) Breaker() *CircuitBreaker { return p.breaker }

// Label is what a run records as its model: the protocol and the host, never
// the path or the secret.
func (p *AGUIProvider) Label() string { return AGUILabel(p.endpoint) }

// AGUILabel is the model label for a remote endpoint: "agui:host".
func AGUILabel(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return string(ProviderAGUI)
	}
	return string(ProviderAGUI) + ":" + u.Host
}

func (p *AGUIProvider) ProviderName() ProviderType { return ProviderAGUI }
func (p *AGUIProvider) SupportsToolCalling() bool  { return true }

// Chat runs the remote with no tools on offer and returns its text. A remote
// that asks for a tool anyway is told, in the returned text, that none were
// offered; the runner's text path never executes anything from prose.
func (p *AGUIProvider) Chat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (string, error) {
	content, calls, err := p.ChatWithTools(ctx, messages, nil, opts)
	if err != nil {
		return "", err
	}
	if len(calls) > 0 {
		names := make([]string, 0, len(calls))
		for _, c := range calls {
			names = append(names, c.Name)
		}
		content = strings.TrimSpace(content + "\n\n[the remote agent asked for " + strings.Join(names, ", ") + "; no tools were offered on this call]")
	}
	return content, nil
}

// ChatStream runs the remote and emits its text as it arrives.
func (p *AGUIProvider) ChatStream(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	out := make(chan StreamChunk, 16)
	go func() {
		defer close(out)
		_, _, err := p.run(ctx, messages, nil, func(delta string) {
			select {
			case out <- StreamChunk{Content: delta}:
			case <-ctx.Done():
			}
		})
		if err != nil {
			out <- StreamChunk{Error: err, Done: true}
			return
		}
		out <- StreamChunk{Done: true}
	}()
	return out, nil
}

// ChatWithTools runs the remote once with our tools on offer and returns what
// it said and what it asked us to run.
func (p *AGUIProvider) ChatWithTools(ctx context.Context, messages []ChatMessage, tools []ToolSpec, _ ChatOptions) (string, []ToolCall, error) {
	return p.run(ctx, messages, tools, nil)
}

// TakeRemoteToolEvents returns, and clears, the tool calls the remote answered
// itself during the last run.
func (p *AGUIProvider) TakeRemoteToolEvents() []RemoteToolEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := p.remote
	p.remote = nil
	return ev
}

// The wire shapes. Field names are AG-UI's, which is camelCase.

type aguiToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function aguiToolFunction `json:"function"`
}

type aguiToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type aguiMessage struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []aguiToolCall `json:"toolCalls,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
}

type aguiTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type aguiRunInput struct {
	ThreadID       string          `json:"threadId"`
	RunID          string          `json:"runId"`
	State          json.RawMessage `json:"state"`
	Messages       []aguiMessage   `json:"messages"`
	Tools          []aguiTool      `json:"tools"`
	Context        []interface{}   `json:"context"`
	ForwardedProps json.RawMessage `json:"forwardedProps"`
}

// aguiEvent is the union of the event fields this client reads. Unknown event
// types are skipped, so a newer remote does not break an older workspace.
//
// Delta is raw because one field name carries two types: a text or tool-args
// delta is a string, and a state delta is a JSON Patch array. Typed as a
// string, a state delta failed to decode and the whole event was dropped,
// which is how a silently frozen agent state starts.
type aguiEvent struct {
	Type         string          `json:"type"`
	MessageID    string          `json:"messageId"`
	Role         string          `json:"role"`
	Delta        json.RawMessage `json:"delta"`
	ToolCallID   string          `json:"toolCallId"`
	ToolCallName string          `json:"toolCallName"`
	Content      json.RawMessage `json:"content"`
	Message      string          `json:"message"`
	Code         string          `json:"code"`
	Snapshot     json.RawMessage `json:"snapshot"`
	Result       json.RawMessage `json:"result"`
}

// aguiOutcome is everything one run's stream said.
type aguiOutcome struct {
	// Text is what the remote said, and Calls are the tool calls it wants this
	// workspace to make.
	Text  string
	Calls []ToolCall
	// Remote is the work it did on its own machine and answered itself.
	Remote []RemoteToolEvent
	// State is the remote's own scratchpad after this run, and StateErr says
	// it could not be kept: a patch that did not apply leaves no state rather
	// than a stale one, because handing an agent a document it never wrote is
	// worse than handing it none.
	State    json.RawMessage
	StateErr error
}

// toAGUIMessages translates the runner's conversation into AG-UI's. Ids are
// positional because the whole history is sent every run and nothing on the
// remote side is keyed by them.
func toAGUIMessages(messages []ChatMessage) []aguiMessage {
	out := make([]aguiMessage, 0, len(messages))
	for i, m := range messages {
		am := aguiMessage{ID: fmt.Sprintf("m%d", i), Role: m.Role, Content: m.Content}
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				am.ToolCalls = append(am.ToolCalls, aguiToolCall{
					ID: tc.ID, Type: "function",
					Function: aguiToolFunction{Name: tc.Name, Arguments: nonEmptyArgs(tc.Arguments)},
				})
			}
		case "tool":
			am.ToolCallID = m.ToolCallID
		case "system", "user", "developer":
		default:
			am.Role = "user"
		}
		out = append(out, am)
	}
	return out
}

func toAGUITools(tools []ToolSpec) []aguiTool {
	out := make([]aguiTool, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, aguiTool{Name: t.Name, Description: t.Description, Parameters: params})
	}
	return out
}

// nonEmptyArgs keeps a tool call's arguments a JSON object, which is what a
// remote deserialises; an empty string is not one.
func nonEmptyArgs(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// applyAuth sets the credential the way the admin configured it: a bearer
// token on Authorization, or the raw secret on a named header (OpenBot's
// bots, for one, read x-openbot-agent-token).
func (p *AGUIProvider) applyAuth(req *http.Request) {
	if p.secret == "" {
		return
	}
	if p.authHeader == "" || strings.EqualFold(p.authHeader, "Authorization") {
		req.Header.Set("Authorization", "Bearer "+p.secret)
		return
	}
	req.Header.Set(p.authHeader, p.secret)
}

// run is one AG-UI run: POST the input, read the stream, return the text and
// the tool calls the remote wants us to make. onText, when set, sees each text
// delta as it arrives.
func (p *AGUIProvider) run(ctx context.Context, messages []ChatMessage, tools []ToolSpec, onText func(string)) (string, []ToolCall, error) {
	if p.endpoint == "" {
		return "", nil, errors.New("agui: no endpoint configured")
	}
	if err := p.breaker.Allow(); err != nil {
		return "", nil, err
	}
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()
	if len(state) == 0 {
		state = json.RawMessage("{}")
	}
	input := aguiRunInput{
		ThreadID:       p.threadID,
		RunID:          uuid.NewString(),
		State:          state,
		Messages:       toAGUIMessages(messages),
		Tools:          toAGUITools(tools),
		Context:        []interface{}{},
		ForwardedProps: json.RawMessage("{}"),
	}
	body, err := json.Marshal(input)
	if err != nil {
		return "", nil, fmt.Errorf("agui: encode run: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("agui: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	p.applyAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		p.breaker.RecordResult(err)
		return "", nil, fmt.Errorf("agui: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("agui: remote answered %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
		if resp.StatusCode == http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %v", ErrProviderRateLimited, err)
		}
		p.breaker.RecordResult(err)
		return "", nil, err
	}

	run, err := readAGUIStream(io.LimitReader(resp.Body, aguiMaxResponseBytes), state, onText)
	if err != nil {
		if errors.Is(err, errNoEventsArrived) {
			err = fmt.Errorf("%w. %s", err, streamSwallowedHint(p.endpoint))
		}
		p.breaker.RecordResult(err)
		return "", nil, err
	}
	p.breaker.RecordSuccess()
	if run.StateErr != nil {
		// Once per endpoint per process: the condition repeats every step of
		// every run until the remote is fixed, and saying so each time would
		// bury the first one.
		if _, said := aguiStateComplaint.LoadOrStore(p.endpoint, struct{}{}); !said {
			helpers.LogErrorWithContext(ctx,
				"agui: %s sent a state patch that could not be applied, so the agent's state is "+
					"being dropped rather than sent back stale: %v", p.Label(), run.StateErr)
		}
	}
	p.mu.Lock()
	p.state = run.State
	p.remote = append(p.remote, run.Remote...)
	p.mu.Unlock()
	// The remote reports no token usage; the run is metered at zero and the
	// spend is the remote's own. Reported so the sink sees a call happened.
	reportUsage(ctx, Usage{})
	return run.Text, run.Calls, nil
}

// errNoEventsArrived is a request the remote answered with a success status and
// no events in it.
var errNoEventsArrived = errors.New("agui: the remote answered but sent no events")

// streamSwallowedHint names the usual cause of that, and the one specific cause
// worth naming outright.
//
// Cloudflare's quick tunnels are the fastest way to put a laptop agent behind
// https, which is exactly what this feature's off-network rule asks for, and
// they do not carry server-sent events at all. So the obvious first attempt at
// using this feature fails in the one way that leaves no trace: the POST
// succeeds, the status is 200, and nothing ever arrives. Naming it costs a
// string and saves somebody an afternoon.
func streamSwallowedHint(endpoint string) string {
	if isQuickTunnel(endpoint) {
		return "Cloudflare quick tunnels (trycloudflare.com) do not carry server-sent events, which is how AG-UI answers. Use a named tunnel, or any address that streams."
	}
	return "Something between this workspace and the agent is buffering or stripping the event stream. Check any proxy, tunnel or CDN in front of it."
}

// isQuickTunnel reports whether an endpoint is on a Cloudflare quick tunnel.
func isQuickTunnel(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "trycloudflare.com" || strings.HasSuffix(h, ".trycloudflare.com")
}

// aguiOpenCall accumulates one tool call across START/ARGS/END events.
type aguiOpenCall struct {
	name string
	args strings.Builder
	// answered is set when the remote emitted TOOL_CALL_RESULT for this call:
	// it ran the tool itself and is not asking us to.
	answered bool
	result   string
}

// readAGUIStream parses an SSE event stream into the run's outcome. Pure over
// its reader, so it is tested with fixtures rather than a server.
//
// state is the remote's scratchpad as it stood before this run, because a
// STATE_DELTA is a patch against it and not against nothing.
//
// A stream that ends without RUN_FINISHED is an error: a run that simply stops
// is the worst failure available here, because it leaves a person waiting with
// no reason given.
func readAGUIStream(r io.Reader, state json.RawMessage, onText func(string)) (aguiOutcome, error) {
	out := aguiOutcome{State: state}
	var sb strings.Builder
	open := map[string]*aguiOpenCall{}
	order := []string{}
	finished := false
	events := 0
	var finishedResult json.RawMessage

	handle := func(ev aguiEvent) error {
		events++
		if events > aguiMaxEvents {
			return errors.New("agui: the remote sent too many events in one run")
		}
		switch ev.Type {
		case "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_CHUNK":
			if d := jsonString(ev.Delta); d != "" {
				sb.WriteString(d)
				if onText != nil {
					onText(d)
				}
			}
		case "TOOL_CALL_START":
			if len(order) >= aguiMaxToolCalls {
				return errors.New("agui: the remote asked for too many tool calls in one run")
			}
			if _, exists := open[ev.ToolCallID]; !exists {
				open[ev.ToolCallID] = &aguiOpenCall{name: ev.ToolCallName}
				order = append(order, ev.ToolCallID)
			}
		case "TOOL_CALL_ARGS":
			if c, ok := open[ev.ToolCallID]; ok {
				c.args.WriteString(jsonString(ev.Delta))
			}
		case "TOOL_CALL_CHUNK":
			// The convenience form: a chunk may open a call, carry a name, and
			// carry arguments, in any combination.
			c, ok := open[ev.ToolCallID]
			if !ok {
				if len(order) >= aguiMaxToolCalls {
					return errors.New("agui: the remote asked for too many tool calls in one run")
				}
				c = &aguiOpenCall{}
				open[ev.ToolCallID] = c
				order = append(order, ev.ToolCallID)
			}
			if ev.ToolCallName != "" {
				c.name = ev.ToolCallName
			}
			c.args.WriteString(jsonString(ev.Delta))
		case "TOOL_CALL_RESULT":
			if c, ok := open[ev.ToolCallID]; ok {
				c.answered = true
				c.result = rawContentString(ev.Content)
			}
		case "STATE_SNAPSHOT":
			// A snapshot re-bases: the remote has just said everything, so a
			// patch that failed earlier no longer matters.
			if len(ev.Snapshot) > 0 {
				out.State, out.StateErr = ev.Snapshot, nil
			}
		case "STATE_DELTA":
			if out.StateErr != nil {
				break // already dropped; nothing to patch
			}
			patched, err := ApplyJSONPatch(out.State, ev.Delta)
			if err != nil {
				out.State, out.StateErr = nil, err
				break
			}
			out.State = patched
		case "RUN_ERROR":
			msg := strings.TrimSpace(ev.Message)
			if msg == "" {
				msg = "the remote agent reported an error"
			}
			if ev.Code != "" {
				msg += " (" + ev.Code + ")"
			}
			return fmt.Errorf("agui: %s", msg)
		case "RUN_FINISHED":
			finished = true
			finishedResult = ev.Result
		}
		return nil
	}

	if err := ForEachSSEFrame(r, aguiMaxResponseBytes, func(data []byte) (bool, error) {
		var ev aguiEvent
		if uerr := json.Unmarshal(data, &ev); uerr != nil {
			return false, nil // not an event we can read; skip rather than fail the run
		}
		return false, handle(ev)
	}); err != nil {
		return aguiOutcome{}, err
	}
	if !finished {
		// Nothing at all is a different fault from stopping partway, and it has
		// a different fix. An answered request carrying no events means
		// something between here and the agent swallowed the stream; a run that
		// started and stopped means the agent did. Reported as the same
		// sentence, the first one is undebuggable.
		if events == 0 {
			return aguiOutcome{}, errNoEventsArrived
		}
		return aguiOutcome{}, errors.New("agui: the remote ended the stream without finishing the run")
	}

	out.Text = sb.String()
	// A run may carry its answer on RUN_FINISHED instead of streaming it. Used
	// only when nothing was streamed, so a remote that does both is not made
	// to repeat itself.
	if strings.TrimSpace(out.Text) == "" {
		if r := strings.TrimSpace(rawContentString(finishedResult)); r != "" && r != "null" {
			out.Text = r
		}
	}

	for _, id := range order {
		c := open[id]
		if c.answered {
			out.Remote = append(out.Remote, RemoteToolEvent{Name: c.name, Arguments: c.args.String(), Result: c.result})
			continue
		}
		if strings.TrimSpace(c.name) == "" {
			continue
		}
		out.Calls = append(out.Calls, ToolCall{ID: id, Name: c.name, Arguments: nonEmptyArgs(c.args.String())})
	}
	return out, nil
}

// jsonString reads a field that the protocol says is a string. Anything else
// reads as empty, so a mistyped delta adds nothing rather than injecting its
// own JSON into somebody's message.
func jsonString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// rawContentString reads a TOOL_CALL_RESULT content or a RUN_FINISHED result,
// which the protocol sends as a string but a remote may send as an object.
func rawContentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
