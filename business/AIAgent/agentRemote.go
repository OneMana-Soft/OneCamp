package business

// A remote brain: what an admin may point an agent at, and how its secret is
// kept.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	maxAGUIAuthHeaderLen = 128
	maxAGUIAuthSecretLen = 8192
)

// validateRemoteBrain checks the endpoint and header name for a remote agent.
// An empty endpoint means no remote brain, and the header is dropped with it
// so a stale name cannot linger on a local agent. The endpoint goes through
// the same front door as a custom model endpoint; the dial-time guard does
// the rest when the run happens.
func validateRemoteBrain(endpoint, header, secret string) (string, string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", "", nil
	}
	if len(secret) > maxAGUIAuthSecretLen {
		return "", "", fmt.Errorf("remote agent secret is too long")
	}
	cleaned, err := ai.ValidateHTTPEndpoint(endpoint, "remote agent endpoint")
	if err != nil {
		return "", "", err
	}
	header = strings.TrimSpace(header)
	if len(header) > maxAGUIAuthHeaderLen {
		return "", "", fmt.Errorf("remote agent auth header name is too long")
	}
	for _, r := range header {
		// A header name is a token: no spaces, no control characters, no
		// colon. Anything else would let a name smuggle a second header.
		if r <= ' ' || r == ':' || r > '~' {
			return "", "", fmt.Errorf("remote agent auth header name is not a valid header name")
		}
	}
	return cleaned, header, nil
}

// validateRemoteProtocol accepts the protocols a remote brain can speak; an
// empty value means AG-UI, which is what every agent meant before A2A.
func validateRemoteProtocol(p string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", model.RemoteAGUI:
		return model.RemoteAGUI, nil
	case model.RemoteA2A:
		return model.RemoteA2A, nil
	}
	return "", fmt.Errorf("remote protocol must be agui or a2a")
}

// fetchA2ACardFn reads an agent card. A seam for tests.
var fetchA2ACardFn = ai.FetchA2ACard

// remoteCardTimeout bounds reading a card when an agent is saved.
const remoteCardTimeout = 15 * time.Second

// resolveRemoteCard reads an A2A agent's card when the agent is saved, so an
// address that is not an A2A agent is refused now, with the reason, rather than
// at its first run in front of the team. Nothing for AG-UI or a local agent.
func resolveRemoteCard(ctx context.Context, protocol, endpoint, header, secret string) (json.RawMessage, error) {
	if protocol != model.RemoteA2A || strings.TrimSpace(endpoint) == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, remoteCardTimeout)
	defer cancel()
	card, err := fetchA2ACardFn(cctx, endpoint, header, secret)
	if err != nil {
		return nil, fmt.Errorf("%s", unwrapDialError(err.Error()))
	}
	return json.Marshal(card)
}

// remoteSecretToStore decides what secret the row keeps.
//
// No endpoint: no secret, whatever was stored, so clearing the remote clears
// its credential. A new secret replaces the old one. An empty secret on an
// agent that has one keeps it: the client never sees the stored value, so an
// edit that did not touch the secret field must not erase it.
func remoteSecretToStore(endpoint, given, stored string) string {
	if strings.TrimSpace(endpoint) == "" {
		return ""
	}
	if given = strings.TrimSpace(given); given != "" {
		return given
	}
	return stored
}

// RemoteBrainCheck is what an admin asked us to try.
type RemoteBrainCheck struct {
	// AgentID, when set, is an agent whose stored secret should be used if no
	// new one is typed. The client never sees a stored secret, so without this
	// an admin could only test an endpoint by retyping a credential they do
	// not have.
	AgentID    string `json:"agent_id"`
	Endpoint   string `json:"endpoint"`
	AuthHeader string `json:"auth_header"`
	AuthSecret string `json:"auth_secret"`
	// Protocol is "agui" (default) or "a2a".
	Protocol string `json:"protocol"`
}

// RemoteBrainResult is what came back, in the words an admin can act on.
type RemoteBrainResult struct {
	OK bool `json:"ok"`
	// Error is why it did not work: the status the remote answered, the
	// message it reported, or the reason this workspace would not dial it.
	Error string `json:"error,omitempty"`
	// Reply is what it said, clipped. Proof it is a remote agent and not an
	// endpoint that happens to accept a POST.
	Reply string `json:"reply,omitempty"`
	// ToolsAsked names tools it tried to call although none were offered.
	// Harmless here, and worth showing: it says the remote is wired to expect
	// tools, which is what it will be given on a real run.
	ToolsAsked []string `json:"tools_asked,omitempty"`
	// Card is what an A2A agent says about itself: name, provider, skills.
	Card *ai.A2ACard `json:"card,omitempty"`
}

// remoteCheckTimeout bounds one check. Long enough for a cold remote to wake
// up, short enough that an admin is not left watching a spinner.
const remoteCheckTimeout = 25 * time.Second

// remoteCheckPrompt is deliberately trivial: this asks whether the endpoint
// speaks the protocol, not whether the agent is any good.
const remoteCheckPrompt = "Reply with one short sentence confirming you received this."

// CheckRemoteBrain tries one AG-UI run against an endpoint and reports what
// happened, so an admin finds out now rather than from a failed run later.
//
// It runs the real provider, so what it proves is what a run would do: the
// same URL rules, the same dial guard, the same local-only refusal, the same
// credential in the same header. A check that used a different client would
// prove something else.
func CheckRemoteBrain(ctx context.Context, in RemoteBrainCheck, actor Actor) (*RemoteBrainResult, error) {
	endpoint, header, err := validateRemoteBrain(in.Endpoint, in.AuthHeader, in.AuthSecret)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		return nil, fmt.Errorf("an endpoint is required")
	}

	secret := strings.TrimSpace(in.AuthSecret)
	if secret == "" && strings.TrimSpace(in.AgentID) != "" {
		stored, serr := storedRemoteSecret(ctx, in.AgentID, actor)
		if serr != nil {
			return nil, serr
		}
		secret = stored
	}

	runCtx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()

	protocol, err := validateRemoteProtocol(in.Protocol)
	if err != nil {
		return nil, err
	}
	var p ai.RemoteBrain
	var card *ai.A2ACard
	if protocol == model.RemoteA2A {
		card, err = fetchA2ACardFn(runCtx, endpoint, header, secret)
		if err != nil {
			return &RemoteBrainResult{OK: false, Error: remoteCheckError(err)}, nil
		}
		p = ai.NewA2AProvider(card.Endpoint, header, secret, "check-"+uuid.NewString(), card.ProtocolVersion)
	} else {
		p = ai.NewAGUIProvider(endpoint, header, secret, "check-"+uuid.NewString())
	}
	// ChatWithTools with nothing on offer, rather than Chat: this run offers no
	// tools either way, and the structured form gives back what the remote
	// asked for instead of a sentence about it.
	reply, calls, err := p.ChatWithTools(runCtx, []ai.ChatMessage{{Role: "user", Content: remoteCheckPrompt}}, nil, ai.ChatOptions{})
	if err != nil {
		return &RemoteBrainResult{OK: false, Error: remoteCheckError(err), Card: card}, nil
	}
	res := &RemoteBrainResult{OK: true, Reply: helpers.TruncateRunes(strings.TrimSpace(reply), 400), Card: card}
	seen := map[string]bool{}
	for _, c := range calls {
		if n := strings.TrimSpace(c.Name); n != "" && !seen[n] {
			seen[n] = true
			res.ToolsAsked = append(res.ToolsAsked, n)
		}
	}
	return res, nil
}

// storedRemoteSecret reads an agent's secret for a check, for an actor allowed
// to manage that agent. It never leaves this package.
func storedRemoteSecret(ctx context.Context, agentID string, actor Actor) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(agentID))
	if err != nil {
		return "", fmt.Errorf("invalid agent id")
	}
	a, err := model.GetAgentByID(ctx, id)
	if err != nil || a == nil {
		return "", errNotFound
	}
	if !canManage(actor, a) {
		return "", errForbidden
	}
	if a.AGUIAuthUnreadable {
		return "", fmt.Errorf("the stored secret cannot be read; enter it again")
	}
	return a.AGUIAuthSecret, nil
}

// unwrapDialError peels the layers off a transport failure until the reason is
// first.
//
// A refused dial arrives as `agui: Post "<url>": agui: <reason>`: this
// package's wrapper, then Go's HTTP client naming the request, then the
// dialer's own wrapper. The admin typed that url and is looking at it; the
// reason is the part they do not have. Peeled in a loop rather than in a fixed
// order because the nesting is not ours to fix and has already changed once.
func unwrapDialError(msg string) string {
	for i := 0; i < 8; i++ {
		before := msg
		msg = strings.TrimPrefix(strings.TrimPrefix(msg, "agui: "), "a2a: ")
		if at := strings.Index(msg, `": `); strings.HasPrefix(msg, `Post "`) && at > 0 {
			msg = msg[at+3:]
		}
		if msg == before {
			return msg
		}
	}
	return msg
}

// remoteCheckError turns a transport failure into the sentence an admin needs.
// The provider's own errors already name the status or the refusal; this adds
// the two cases where the reason is ours rather than the remote's, because
// "connection refused" does not tell anybody that local-only mode is on.
func remoteCheckError(err error) string {
	msg := unwrapDialError(err.Error())
	switch {
	case strings.Contains(msg, "local-only"):
		return "Local-only AI mode is on, so only endpoints on your own network can be reached. " + msg
	case strings.Contains(msg, "blocked for security"):
		return "That address is blocked: it is a cloud metadata or link-local address, which no model or agent endpoint should be on."
	}
	return msg
}
