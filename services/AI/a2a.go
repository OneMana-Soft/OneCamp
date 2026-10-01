package ai

// A2A: any agent that speaks the Agent2Agent protocol, as a teammate.
//
// WHY. Agents are arriving from everywhere (a team's own, a vendor's, Claude,
// Codex, Gemini), and A2A, now a Linux Foundation project, is how they are
// reached. An A2A agent is wired in exactly the way an AG-UI one is: as the
// brain of a OneCamp agent, a provider to the one runner. So it gets the same
// channel scope, the same allow-listed tools, the same audit and the same
// person accountable for it, without a second loop that could drift.
//
// WHAT IS SENT. A2A has no way to hand a remote our tools, so the remote gets
// the conversation it was brought into, as one message, and answers in text.
// It may use its own tools; they are its business and its bill. Anything it
// wants done in this workspace goes through the MCP server under its own
// token, where every call is checked against the person behind it.
//
// DIALECTS. Version 1.0 renamed the methods (SendMessage, GetTask), the roles
// (ROLE_USER) and wrapped results ({"task": ...}); 0.3, still common, used
// message/send, "user" and a "kind" field. The agent card says which it
// speaks, and both are handled.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// ProviderA2A labels runs whose reasoning happened at an A2A agent.
const ProviderA2A ProviderType = "a2a"

const (
	a2aMaxResponseBytes = 4 << 20
	a2aMaxCardBytes     = 256 << 10
	// a2aPollEvery and a2aPollFor bound waiting on a task the remote accepted
	// but has not finished. The run's own deadline still applies on top.
	a2aPollEvery = 1500 * time.Millisecond
	a2aPollFor   = 5 * time.Minute
	// What of the conversation goes along: the recent turns, each clipped, so a
	// long thread cannot become an unbounded request.
	a2aMaxTurns     = 12
	a2aMaxTurnChars = 4000
)

// RemoteBrain is what the runner needs from any remote reasoning endpoint.
type RemoteBrain interface {
	LLMProvider
	ToolCallingProvider
	Breaker() *CircuitBreaker
	Label() string
}

var (
	_ RemoteBrain = (*AGUIProvider)(nil)
	_ RemoteBrain = (*A2AProvider)(nil)
)

// A2ACard is what an agent card says about the agent, trimmed to what this
// workspace shows and uses.
type A2ACard struct {
	Name            string     `json:"name"`
	Description     string     `json:"description,omitempty"`
	Version         string     `json:"version,omitempty"`
	ProtocolVersion string     `json:"protocol_version,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	Endpoint        string     `json:"endpoint"`
	Skills          []A2ASkill `json:"skills,omitempty"`
	Streaming       bool       `json:"streaming,omitempty"`
}

// A2ASkill is one thing the agent says it can do.
type A2ASkill struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// rawCard is the published document, in every shape seen in the wild.
type rawCard struct {
	Name               string `json:"name"`
	Description        string `json:"description"`
	URL                string `json:"url"`
	Version            string `json:"version"`
	ProtocolVersion    string `json:"protocolVersion"`
	PreferredTransport string `json:"preferredTransport"`
	Provider           struct {
		Organization string `json:"organization"`
	} `json:"provider"`
	Capabilities struct {
		Streaming bool `json:"streaming"`
	} `json:"capabilities"`
	AdditionalInterfaces []rawInterface `json:"additionalInterfaces"`
	SupportedInterfaces  []rawInterface `json:"supportedInterfaces"`
	Skills               []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"skills"`
}

type rawInterface struct {
	URL             string `json:"url"`
	Transport       string `json:"transport"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// jsonRPCEndpoint picks the card's JSON-RPC endpoint, the binding this client
// speaks, and the protocol version that goes with it. Pure.
func (c rawCard) jsonRPCEndpoint() (string, string, error) {
	isRPC := func(s string) bool {
		s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
		return s == "JSONRPC" || s == "JSONRPC2.0"
	}
	if strings.TrimSpace(c.URL) != "" && (c.PreferredTransport == "" || isRPC(c.PreferredTransport)) {
		return strings.TrimSpace(c.URL), c.ProtocolVersion, nil
	}
	for _, list := range [][]rawInterface{c.SupportedInterfaces, c.AdditionalInterfaces} {
		for _, i := range list {
			if isRPC(i.Transport) || isRPC(i.ProtocolBinding) {
				v := i.ProtocolVersion
				if v == "" {
					v = c.ProtocolVersion
				}
				return strings.TrimSpace(i.URL), v, nil
			}
		}
	}
	offered := c.PreferredTransport
	if offered == "" {
		offered = "no endpoint"
	}
	return "", "", fmt.Errorf("a2a: this agent offers %s; OneCamp speaks A2A over JSON-RPC", offered)
}

// cardCandidates are the places an agent card may be, given what an admin
// typed: the card itself, or the address the agent lives at. Pure.
func cardCandidates(given string) []string {
	given = strings.TrimSpace(given)
	u, err := url.Parse(given)
	if err != nil || u.Host == "" {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		return []string{given}
	}
	base := u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
	origin := u.Scheme + "://" + u.Host
	out := []string{base + "/.well-known/agent-card.json", base + "/.well-known/agent.json"}
	if base != origin {
		out = append(out, origin+"/.well-known/agent-card.json", origin+"/.well-known/agent.json")
	}
	return out
}

// isV1 says whether a protocol version is 1.0 or later. Pure.
func isV1(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 1
}

// newA2AClient is the same guarded client every remote gets: the dial-time
// address guard, and the refusal to send workspace content off the customer's
// network in the clear or without a credential.
func newA2AClient(endpoint, secret string) *http.Client {
	return newProviderHTTPClient(httpClientConfig{
		timeout:         0,
		guardSSRF:       true,
		offLocalRefusal: strings.Replace(OffLocalRefusal(endpoint, secret), "agui:", "a2a:", 1),
	})
}

func applyRemoteAuth(req *http.Request, header, secret string) {
	if secret == "" {
		return
	}
	if header == "" || strings.EqualFold(header, "Authorization") {
		req.Header.Set("Authorization", "Bearer "+secret)
		return
	}
	req.Header.Set(header, secret)
}

// FetchA2ACard finds and reads an agent card. given is the card's own address
// or the agent's; given must already have passed ValidateHTTPEndpoint, and the
// endpoint the card names is checked the same way before it is returned.
func FetchA2ACard(ctx context.Context, given, authHeader, secret string) (*A2ACard, error) {
	candidates := cardCandidates(given)
	if len(candidates) == 0 {
		return nil, errors.New("a2a: not an address")
	}
	client := newA2AClient(given, secret)
	var lastErr error
	for _, cu := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cu, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		applyRemoteAuth(req, authHeader, secret)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, a2aMaxCardBytes))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("a2a: %s answered %d", cu, resp.StatusCode)
			continue
		}
		var rc rawCard
		if err := json.Unmarshal(body, &rc); err != nil || strings.TrimSpace(rc.Name) == "" {
			lastErr = fmt.Errorf("a2a: %s is not an agent card", cu)
			continue
		}
		return cardFromRaw(rc)
	}
	if lastErr == nil {
		lastErr = errors.New("a2a: no agent card found")
	}
	return nil, fmt.Errorf("a2a: could not read the agent card: %w", lastErr)
}

func cardFromRaw(rc rawCard) (*A2ACard, error) {
	endpoint, version, err := rc.jsonRPCEndpoint()
	if err != nil {
		return nil, err
	}
	cleaned, err := ValidateHTTPEndpoint(endpoint, "agent card endpoint")
	if err != nil {
		return nil, err
	}
	card := &A2ACard{
		Name: strings.TrimSpace(rc.Name), Description: clip(rc.Description, 600), Version: rc.Version,
		ProtocolVersion: version, Provider: rc.Provider.Organization, Endpoint: cleaned, Streaming: rc.Capabilities.Streaming,
	}
	for i, s := range rc.Skills {
		if i == 20 {
			break
		}
		name := strings.TrimSpace(s.Name)
		if name == "" {
			name = s.ID
		}
		card.Skills = append(card.Skills, A2ASkill{ID: s.ID, Name: clip(name, 120), Description: clip(s.Description, 300)})
	}
	return card, nil
}

func clip(s string, n int) string {
	return helpers.TruncateRunesWithSuffix(strings.TrimSpace(s), n, "…")
}

// A2AProvider is one A2A agent, reached over JSON-RPC.
type A2AProvider struct {
	endpoint   string
	authHeader string
	secret     string
	contextID  string
	v1         bool
	client     *http.Client
	breaker    *CircuitBreaker
}

// NewA2AProvider builds a provider for one agent. endpoint is the card's
// JSON-RPC endpoint; contextID is the A2A conversation, the run id so runs
// never share a history on the remote; protocolVersion comes from the card.
func NewA2AProvider(endpoint, authHeader, secret, contextID, protocolVersion string) *A2AProvider {
	endpoint = strings.TrimSpace(endpoint)
	return &A2AProvider{
		endpoint: endpoint, authHeader: strings.TrimSpace(authHeader), secret: secret,
		contextID: strings.TrimSpace(contextID), v1: isV1(protocolVersion),
		client: newA2AClient(endpoint, secret), breaker: aguiBreakerFor("a2a " + endpoint),
	}
}

func (p *A2AProvider) Breaker() *CircuitBreaker   { return p.breaker }
func (p *A2AProvider) Label() string              { return RemoteLabel(ProviderA2A, p.endpoint) }
func (p *A2AProvider) ProviderName() ProviderType { return ProviderA2A }
func (p *A2AProvider) SupportsToolCalling() bool  { return true }

// RemoteLabel is what a run records as its model: the protocol and the host,
// never the path or the secret.
func RemoteLabel(kind ProviderType, endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return string(kind)
	}
	return string(kind) + ":" + u.Host
}

func (p *A2AProvider) Chat(ctx context.Context, messages []ChatMessage, _ ChatOptions) (string, error) {
	return p.send(ctx, messages)
}

func (p *A2AProvider) ChatStream(ctx context.Context, messages []ChatMessage, _ ChatOptions) (<-chan StreamChunk, error) {
	out := make(chan StreamChunk, 2)
	go func() {
		defer close(out)
		text, err := p.send(ctx, messages)
		if err != nil {
			out <- StreamChunk{Error: err, Done: true}
			return
		}
		out <- StreamChunk{Content: text}
		out <- StreamChunk{Done: true}
	}()
	return out, nil
}

// ChatWithTools never returns tool calls: A2A cannot carry our tools, so the
// answer is always final.
func (p *A2AProvider) ChatWithTools(ctx context.Context, messages []ChatMessage, _ []ToolSpec, _ ChatOptions) (string, []ToolCall, error) {
	text, err := p.send(ctx, messages)
	return text, nil, err
}

// a2aPrompt is the conversation as one message: recent turns, oldest first,
// clipped, and system text left out (it is written for our own models and
// names tools the remote does not have). Pure.
func a2aPrompt(messages []ChatMessage) string {
	var turns []ChatMessage
	for _, m := range messages {
		if m.Role == "user" || m.Role == "assistant" {
			if strings.TrimSpace(m.Content) != "" {
				turns = append(turns, m)
			}
		}
	}
	if len(turns) > a2aMaxTurns {
		turns = turns[len(turns)-a2aMaxTurns:]
	}
	if len(turns) == 1 {
		return clip(turns[0].Content, a2aMaxTurnChars)
	}
	var b strings.Builder
	for i, m := range turns {
		if i == len(turns)-1 {
			b.WriteString("\nThe request now:\n")
		} else if i == 0 {
			b.WriteString("Earlier in this conversation:\n")
		}
		who := "Person"
		if m.Role == "assistant" {
			who = "You"
		}
		b.WriteString(who + ": " + clip(m.Content, a2aMaxTurnChars) + "\n")
	}
	return strings.TrimSpace(b.String())
}

// sendRequest is the JSON-RPC body for one message, in the remote's dialect. Pure.
func sendRequest(v1 bool, contextID, text string) map[string]any {
	msg := map[string]any{"messageId": uuid.NewString(), "contextId": contextID}
	if v1 {
		msg["role"] = "ROLE_USER"
		msg["parts"] = []any{map[string]any{"text": text}}
		return rpc("SendMessage", map[string]any{"message": msg, "configuration": map[string]any{"returnImmediately": false}})
	}
	msg["kind"] = "message"
	msg["role"] = "user"
	msg["parts"] = []any{map[string]any{"kind": "text", "text": text}}
	return rpc("message/send", map[string]any{"message": msg, "configuration": map[string]any{"blocking": true}})
}

func rpc(method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": uuid.NewString(), "method": method, "params": params}
}

// a2aObject is a Task or a Message, whichever came back.
type a2aObject struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	Parts     []a2aPart  `json:"parts"`
	Status    *a2aStatus `json:"status"`
	Artifacts []struct {
		Parts []a2aPart `json:"parts"`
	} `json:"artifacts"`
	// 1.0 wraps the result.
	Task    *a2aObject `json:"task"`
	Message *a2aObject `json:"message"`
}

type a2aPart struct {
	Text string `json:"text"`
}

type a2aStatus struct {
	State   string     `json:"state"`
	Message *a2aObject `json:"message"`
}

// state is a task state in one spelling: "completed", "input-required". Pure.
func a2aState(s string) string {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "TASK_STATE_"))
	return strings.ReplaceAll(s, "_", "-")
}

func partsText(parts []a2aPart) string {
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n\n")
}

// unwrap returns the task or message inside a result, whatever the dialect.
func (o *a2aObject) unwrap() *a2aObject {
	if o.Task != nil {
		return o.Task
	}
	if o.Message != nil {
		return o.Message
	}
	return o
}

// outcome reads a result: the text to post, whether the task is still going,
// and the error when it failed. Pure.
func (o *a2aObject) outcome() (text string, pending bool, err error) {
	r := o.unwrap()
	if r.Status == nil {
		return partsText(r.Parts), false, nil // a Message
	}
	status := ""
	if r.Status.Message != nil {
		status = partsText(r.Status.Message.Parts)
	}
	var arts []string
	for _, a := range r.Artifacts {
		if t := partsText(a.Parts); t != "" {
			arts = append(arts, t)
		}
	}
	switch a2aState(r.Status.State) {
	case "submitted", "working":
		return "", true, nil
	case "failed", "rejected", "canceled":
		if status == "" {
			status = "no reason given"
		}
		return "", false, fmt.Errorf("a2a: the agent %s the task: %s", a2aState(r.Status.State), status)
	case "input-required", "auth-required":
		return helpers.FirstNonBlank(status, strings.Join(arts, "\n\n"), "The agent needs more information to continue."), false, nil
	default: // completed, or a state this client does not know: show what came back
		return helpers.FirstNonBlank(strings.Join(arts, "\n\n"), status), false, nil
	}
}

func (p *A2AProvider) call(ctx context.Context, body map[string]any) (*a2aObject, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.v1 {
		req.Header.Set("A2A-Version", "1.0")
	}
	applyRemoteAuth(req, p.authHeader, p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, a2aMaxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("a2a: remote answered %d: %s", resp.StatusCode, clip(string(data), 300))
		if resp.StatusCode == http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %v", ErrProviderRateLimited, err)
		}
		return nil, err
	}
	var env struct {
		Result *a2aObject `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("a2a: the remote's answer is not JSON-RPC: %s", clip(string(data), 200))
	}
	if env.Error != nil {
		return nil, fmt.Errorf("a2a: remote error %d: %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil {
		return nil, errors.New("a2a: the remote answered with no result")
	}
	return env.Result, nil
}

// send delivers the conversation and returns the agent's answer, waiting for
// a task it accepted but has not finished.
func (p *A2AProvider) send(ctx context.Context, messages []ChatMessage) (string, error) {
	if p.endpoint == "" {
		return "", errors.New("a2a: no endpoint configured")
	}
	if err := p.breaker.Allow(); err != nil {
		return "", err
	}
	text := a2aPrompt(messages)
	if text == "" {
		return "", errors.New("a2a: nothing to send")
	}
	res, err := p.call(ctx, sendRequest(p.v1, p.contextID, text))
	if err != nil {
		p.breaker.RecordResult(err)
		return "", err
	}
	deadline := time.Now().Add(a2aPollFor)
	for {
		out, pending, oerr := res.outcome()
		if oerr != nil {
			p.breaker.RecordSuccess() // the remote works; it declined this task
			return "", oerr
		}
		if !pending {
			p.breaker.RecordSuccess()
			reportUsage(ctx, Usage{})
			if strings.TrimSpace(out) == "" {
				return "", errors.New("a2a: the agent finished with nothing to say")
			}
			return out, nil
		}
		id := res.unwrap().ID
		if id == "" || time.Now().After(deadline) {
			return "", errors.New("a2a: the agent did not finish in time")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(a2aPollEvery):
		}
		method := "tasks/get"
		if p.v1 {
			method = "GetTask"
		}
		if res, err = p.call(ctx, rpc(method, map[string]any{"id": id})); err != nil {
			p.breaker.RecordResult(err)
			return "", err
		}
	}
}
