package business

// "Connect a model provider" as a setup step that tells the truth.
//
// WHY IT MOVED HERE. The onboarding package used to define this step itself and
// mark it done when the feature registry reported AI as enabled. The registry
// reports enabled whenever a client object exists, and a client object exists on
// every fresh install: the shipped defaults point at an Ollama engine that is not
// started by default, and building the client does not dial it. So the first
// screen a paying admin saw ticked "Connect a model provider" while every AI
// request they could make would fail.
//
// Registered from here through the same hook the drill uses, because the
// onboarding package is compiled into the AI-free edition and cannot import this
// one. Linking the AI packages is what puts the step on the list, and the
// onboarding package now knows nothing about AI at all.

import (
	"context"
	"time"

	onboarding "github.com/akashc777/OneCamp/business/Onboarding"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// ProviderStepID is the step's id, exported for the test that checks it is on
// the list.
const ProviderStepID = "ai"

// ProviderStepWeight places the step among the contributed ones: first, because
// everything else the AI packages add is about what the provider then does.
const ProviderStepWeight = 10

const (
	// providerProbeTTL bounds how often the checklist dials the provider. The
	// home page asks for the checklist on every load, and an uncached probe
	// would put a model-provider round trip in front of every admin's home.
	providerProbeTTL = time.Minute
	// providerProbeTimeout keeps one probe from holding a home page open. The
	// admin "Test connection" button allows fifteen seconds because a person
	// pressed it and is waiting for that answer; nobody asked for this one.
	providerProbeTimeout = 2 * time.Second
	providerProbeKey     = "active-chat-provider"
)

// providerReachable caches the last probe result, a negative one included, so a
// provider that is down costs one two-second wait per minute rather than one per
// page.
var providerReachable = helpers.NewTTLCache[bool](providerProbeTTL)

// providerStep is what gets registered. A named value rather than a literal in
// init so the test can read the definition without a registry accessor that
// nothing in production would call.
var providerStep = onboarding.Step{
	ID:     ProviderStepID,
	Title:  "Connect a model provider",
	Detail: "Bring your own key, or run a local model. Nothing leaves your server without it.",
	Href:   "/app/admin?tab=ai-models",
	// OneCamp Cloud runs a local model for a workspace it runs.
	SelfHostedOnly: true,
}

func init() {
	// nil include: this package being linked is the whole condition.
	onboarding.Register(providerStep, ProviderStepWeight, nil,
		func(ctx context.Context, _ userModels.UserInfo) bool {
			return ActiveChatProviderAnswers(ctx)
		})
}

// ActiveChatProviderAnswers reports whether the provider this workspace is
// configured to send chat to actually answers a request.
//
// The question the step is really asking. "Enabled" and "a client exists" are
// both true on an install where nothing works; what a person means by
// "connected" is that a request comes back.
func ActiveChatProviderAnswers(ctx context.Context) bool {
	if cached, ok := providerReachable.Get(providerProbeKey); ok {
		return cached
	}
	ok := probeActiveChatProvider(ctx)
	providerReachable.Set(providerProbeKey, ok)
	return ok
}

// probeActiveChatProvider lists the active provider's models, the same call the
// admin "Test connection" button makes. Listing is the right probe because it
// exercises what a request needs, the host, the port and the credential,
// without spending tokens on a completion.
func probeActiveChatProvider(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, providerProbeTimeout)
	defer cancel()

	settings, err := aiModels.GetSettings(ctx)
	if err != nil || !providerSelected(settings) {
		return false
	}
	p, err := aiModels.GetProvider(ctx, *settings.ChatProviderID)
	if err != nil || p == nil || !p.Enabled {
		return false
	}
	client, err := buildProviderClient(p)
	if err != nil {
		return false
	}
	_, err = client.lister.ListModels(ctx)
	return err == nil
}

// ForgetProviderProbe drops the cached answer, so a provider an admin has just
// saved is re-checked on the next look rather than a minute later. Called from
// the reload path, which is the one place the answer is known to have changed.
func ForgetProviderProbe() {
	providerReachable.Delete(providerProbeKey)
}

// providerSelected is the part of the probe that needs no network: AI is on and
// a chat provider is chosen. Without both there is nothing to dial, and dialling
// nothing must read as "not connected" rather than as a timeout.
func providerSelected(settings *aiModels.AISettings) bool {
	return settings != nil && settings.Enabled && settings.ChatProviderID != nil
}
