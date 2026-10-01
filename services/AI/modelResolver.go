package ai

// Per-user model resolution (migration 81).
//
// The workspace has ONE default chat model (svc.LLM). A member may pick a
// different admin-authorized model for their personal assistant. Because that
// pick can target a DIFFERENT provider (e.g. default Ollama, pick OpenAI),
// setting opts.Model on the default client is not enough - we need a client
// built for the picked provider+model. modelResolver builds those clients
// lazily and caches them per service instance, alongside a per-model circuit
// breaker so a failing exotic model cannot trip the breaker that guards the
// workspace default.
//
// The cache lives on the AIService instance, so a config reload (which builds
// a fresh service and swaps it atomically) transparently discards stale
// clients; in-flight requests keep the instance they captured.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

var errServiceDisabled = errors.New("ai: service is not enabled")

// resolvedModel pairs a built LLM client with its own circuit breaker.
type resolvedModel struct {
	llm LLMProvider
	cb  *CircuitBreaker
	// isCloud is the endpoint's local/cloud classification, computed ONCE at
	// build time (a DNS resolution) and cached for the service lifetime so the
	// per-request local-only check on the chat hot path never pays DNS cost.
	isCloud bool
	// limits are THIS model's token limits, resolved once at build time and
	// cached with the client. Cached together deliberately: the window is baked
	// into the client (Ollama's num_ctx is set at construction), so a client and
	// a window that disagree is not a thing that should be representable.
	limits ModelLimits
}

// modelResolver caches non-default model clients for the life of one service
// instance. Safe for concurrent use.
type modelResolver struct {
	mu      sync.Mutex
	entries map[string]*resolvedModel
	// auditedBlocks dedupes local-only egress audit rows so a member with a
	// cloud pick doesn't write one row per message — at most one per
	// (user, model) for the life of this service instance.
	auditedBlocks sync.Map
}

func newModelResolver() *modelResolver {
	return &modelResolver{entries: make(map[string]*resolvedModel)}
}

// get returns a cached or freshly-built client+breaker for ep. The endpoint is
// built by the caller (one DB read) and passed in; cfg supplies the context
// window and reasoning flag so the picked model runs with the same tuning as
// the default.
// limitsFor is called ONLY on a cache miss and supplies this model's own limits. A
// function rather than a value because resolving it reads the model's allowlist row: this
// cache almost always hits on the hot path, and paying a database round trip per request
// for something already cached beside the client would be a poor trade. nil means "use
// the workspace window", which is what every caller did before per-model limits existed.
func (mc *modelResolver) get(key string, ep Endpoint, cfg *AIConfig, limitsFor func() ModelLimits) (*resolvedModel, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if e, ok := mc.entries[key]; ok {
		return e, nil
	}
	// Derived from the cfg this resolver was handed rather than the global config
	// singleton. They are the same object in production — the service is built from it —
	// but reading the global here would make the window depend on state this function was
	// not given, which is both harder to reason about and untestable.
	lim := limitsForEndpoint(ep, cfg)
	if limitsFor != nil {
		lim = limitsFor()
	}
	// The window goes into the CLIENT, not just the budget. Ollama applies num_ctx at
	// construction, so a model allowed with a 128k window would otherwise be built with
	// the workspace's (often 8192) and genuinely RUN small — the prompt budget being
	// wrong is only the visible half of that.
	llm, err := buildLLM(ep, lim.ContextWindow, lim.MaxOutput, cfg.ReasoningEnabled, cfg.redactorForEndpoint(ep))
	if err != nil {
		return nil, err
	}
	// Each non-default model gets its own breaker with the same thresholds as
	// the default ResiliencyManager, so health is tracked independently.
	// isCloud (a DNS resolution) is only needed by the local-only egress
	// check, so classify ONLY when local-only mode is on — the common path
	// (local-only off) pays no DNS cost.
	isCloud := false
	if cfg.LocalOnlyMode {
		isCloud = cfg.endpointIsCloud(ep)
	}
	e := &resolvedModel{llm: llm, cb: NewCircuitBreaker(5, 30*time.Second), isCloud: isCloud, limits: lim}
	mc.entries[key] = e
	return e, nil
}

// limitsFromAllowlist returns a limits resolver for a (provider, model) pair, reading
// what the admin recorded on its allowlist row and falling back to the workspace window
// when the pair is absent or unreadable.
//
// Never fails the call: a database hiccup here degrades to the window every model used
// before migration 140, which is the behaviour this replaced rather than a new failure
// mode.
func limitsFromAllowlist(ctx context.Context, providerID uuid.UUID, model string) func() ModelLimits {
	return func() ModelLimits {
		am, err := aiModels.GetAuthorizedModelByProviderModel(ctx, providerID, model)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "AI: limits lookup for %s/%s failed, using the workspace window: %v", providerID, model, err)
		}
		if err != nil || am == nil {
			return LimitsForModel("", model, 0, 0)
		}
		return LimitsForModel(am.ProviderKind, am.Model, am.ContextWindowTokens, am.MaxOutputTokens)
	}
}

// ResolveUserModel returns the LLM client and circuit breaker to use for a
// request on behalf of userUUID: the user's chosen authorized model when set
// and usable, otherwise the workspace default. It never returns nil for an
// enabled service. Rate limiting is intentionally NOT handled here - it is
// per-user and model-independent, so callers keep using svc.Resiliency for
// the rate-limit check.
func (s *AIService) ResolveUserModel(ctx context.Context, userUUID string) (LLMProvider, *CircuitBreaker) {
	llm, cb, _ := s.ResolveUserModelWithLimits(ctx, userUUID)
	return llm, cb
}

// ResolveUserModelWithLimits is ResolveUserModel plus the TOKEN LIMITS of the model
// it actually returned.
//
// Every fallback path in here answers with the workspace default client, and each one
// must therefore answer with the workspace LIMITS — a member's 200k pick that was
// refused for residency must not leave a caller budgeting 200k for the 8k local model
// that replaced it. Pairing the two in one return value is what makes that hard to get
// wrong; the alternative is a second lookup at the call site that can disagree with
// the first, silently, on exactly the paths that already degraded once.
func (s *AIService) ResolveUserModelWithLimits(ctx context.Context, userUUID string) (LLMProvider, *CircuitBreaker, ModelLimits) {
	if s == nil || !s.IsEnabled() {
		if s != nil && s.Resiliency != nil {
			return s.LLM, s.Resiliency.CB, WorkspaceLimits()
		}
		return s.llmOrNil(), nil, WorkspaceLimits()
	}

	defLLM, defCB, defLimits := s.LLM, s.Resiliency.CB, WorkspaceLimits()
	if userUUID == "" || s.models == nil {
		return defLLM, defCB, defLimits
	}

	pref, err := aiModels.GetUserModelPreference(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: user model preference lookup failed for %s, using default: %v", userUUID, err)
		return defLLM, defCB, defLimits
	}
	if pref == nil {
		return defLLM, defCB, defLimits // no pick / revoked / disabled -> workspace default
	}

	providerID := pref.ProviderID
	ep, err := endpointFromProvider(ctx, &providerID, pref.Model, 0, s.Config)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: cannot resolve picked model %s/%s, using default: %v", pref.ProviderName, pref.Model, err)
		return defLLM, defCB, defLimits
	}

	// If the pick resolves to the same endpoint as the workspace default,
	// reuse the default client + breaker (no duplicate client, and the admin's
	// circuit-state view stays accurate). The MODEL is still the pick, though, so its
	// own limits apply — the endpoint being shared says nothing about the window an
	// admin recorded for it.
	if endpointsEqual(ep, s.Config.Chat) {
		return defLLM, defCB, LimitsForModel(pref.ProviderKind, pref.Model, pref.ContextWindowTokens, pref.MaxOutputTokens)
	}

	// pref IS the allowlist row, so this model's limits are already in hand — no
	// lookup needed for a member's pick.
	rm, err := s.models.get(providerID.String()+"|"+pref.Model, ep, s.Config, func() ModelLimits {
		return LimitsForModel(pref.ProviderKind, pref.Model, pref.ContextWindowTokens, pref.MaxOutputTokens)
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: building picked model %s/%s failed, using default: %v", pref.ProviderName, pref.Model, err)
		return defLLM, defCB, defLimits
	}
	// Under local-only mode, a member's cloud model pick (authorized before the
	// mode was turned on) is refused at the dial layer. Here we degrade
	// gracefully to the workspace default (guaranteed local) and record a
	// content-free audit row, deduped per (user, model) so it can't flood.
	if s.Config.LocalOnlyMode && rm.isCloud {
		if _, seen := s.models.auditedBlocks.LoadOrStore(userUUID+"|"+pref.Model, true); !seen {
			auditEgressBlocked(ctx, "chat", ep.Kind)
		}
		helpers.LogInfoWithContext(ctx, "AI: local-only mode ignored cloud model pick %s/%s for user %s; using local default", pref.ProviderName, pref.Model, userUUID)
		return defLLM, defCB, defLimits
	}
	return rm.llm, rm.cb, rm.limits
}

func (s *AIService) llmOrNil() LLMProvider {
	if s == nil {
		return nil
	}
	return s.LLM
}

// ClientForModel returns an LLM client for an authorized (provider, model),
// building and caching it like the per-user path. Used by the admin self-test
// to exercise a specific model on demand. Returns the workspace default client
// when the pick resolves to the same endpoint.
func (s *AIService) ClientForModel(ctx context.Context, providerID uuid.UUID, model string) (LLMProvider, error) {
	if s == nil || !s.IsEnabled() || s.models == nil {
		return nil, errServiceDisabled
	}
	ep, err := endpointFromProvider(ctx, &providerID, model, 0, s.Config)
	if err != nil {
		return nil, err
	}
	if endpointsEqual(ep, s.Config.Chat) {
		return s.LLM, nil
	}
	// Shares the cache key with ResolveExplicitModel, so it must resolve limits the
	// same way — otherwise whichever path warmed the entry first would decide the
	// window, and the admin self-test would exercise a differently-built client
	// than the one that serves traffic.
	rm, err := s.models.get(providerID.String()+"|"+model, ep, s.Config, limitsFromAllowlist(ctx, providerID, model))
	if err != nil {
		return nil, err
	}
	return rm.llm, nil
}

// ResolveExplicitModel returns the LLM client + circuit breaker for a SPECIFIC
// authorized (provider, model) — used by an AI agent that runs on its own
// chosen model rather than a member's preference. It mirrors ResolveUserModel's
// safety posture so a misconfigured or unavailable agent model never hard-fails
// a run:
//   - returns the workspace default (client + breaker) when the service is
//     disabled, the endpoint can't be built, or the pick resolves to the
//     default endpoint;
//   - under local-only mode, refuses a cloud endpoint and degrades to the local
//     default, recording a content-free egress-blocked audit row (residency);
//   - gives the picked model its OWN circuit breaker so a failing exotic model
//     can't trip the breaker guarding the workspace default or other agents.
//
// The bool result reports whether the default was used as a fallback (for
// caller logging), never an error — resolution is always safe.
func (s *AIService) ResolveExplicitModel(ctx context.Context, providerID uuid.UUID, model string) (LLMProvider, *CircuitBreaker, bool) {
	llm, cb, usedDefault, _ := s.ResolveExplicitModelWithLimits(ctx, providerID, model)
	return llm, cb, usedDefault
}

// ResolveExplicitModelWithLimits is ResolveExplicitModel plus the TOKEN LIMITS of the
// model it actually returned. Every degraded path answers with the workspace default
// client and therefore with the workspace limits — see ResolveUserModelWithLimits for
// why the two are returned together rather than looked up separately.
func (s *AIService) ResolveExplicitModelWithLimits(ctx context.Context, providerID uuid.UUID, model string) (LLMProvider, *CircuitBreaker, bool, ModelLimits) {
	if s == nil || !s.IsEnabled() || s.models == nil || s.Resiliency == nil {
		return s.llmOrNil(), nil, true, WorkspaceLimits()
	}
	defLLM, defCB, defLimits := s.LLM, s.Resiliency.CB, WorkspaceLimits()

	ep, err := endpointFromProvider(ctx, &providerID, model, 0, s.Config)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: agent model %s/%s unresolved, using default: %v", providerID, model, err)
		return defLLM, defCB, true, defLimits
	}
	// Same endpoint as the workspace default → reuse the default client/breaker
	// so the admin's circuit view stays accurate and we don't duplicate clients.
	// The requested MODEL still governs the budget: sharing an endpoint with the
	// default says nothing about the window recorded for this model.
	if endpointsEqual(ep, s.Config.Chat) {
		return defLLM, defCB, false, limitsFromAllowlist(ctx, providerID, model)()
	}
	rm, err := s.models.get(providerID.String()+"|"+model, ep, s.Config, limitsFromAllowlist(ctx, providerID, model))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: building agent model %s/%s failed, using default: %v", providerID, model, err)
		return defLLM, defCB, true, defLimits
	}
	// Residency: under local-only mode an agent's cloud model is refused at the
	// dial layer anyway; degrade here to the guaranteed-local default and record
	// a content-free audit row so the block is visible.
	if s.Config.LocalOnlyMode && rm.isCloud {
		auditEgressBlocked(ctx, "agent", ep.Kind)
		helpers.LogInfoWithContext(ctx, "AI: local-only mode ignored agent cloud model %s/%s; using local default", providerID, model)
		return defLLM, defCB, true, defLimits
	}
	return rm.llm, rm.cb, false, rm.limits
}

// endpointsEqual reports whether two chat endpoints target the same model on
// the same provider connection (kind + base URL + model name).
func endpointsEqual(a, b Endpoint) bool {
	return a.Kind == b.Kind && a.BaseURL == b.BaseURL && a.Model == b.Model
}

// ResolveFallbackModel returns a SECONDARY llm + breaker to use when the
// primary model is rate-limited / quota-exhausted (HTTP 429), so a run degrades
// to the backup instead of hard-failing. The fallback is the workspace's local
// Ollama chat model (env-configured OllamaHost/OllamaModel) — a self-hosted
// provider with no per-day / per-minute cap, which is exactly what you want
// when a cloud free-tier quota is spent. Returns (nil, nil) when no distinct
// local model is configured (nothing to fall back to) or the primary already
// IS that local model. Cached per service instance via the model resolver.
func (s *AIService) ResolveFallbackModel(ctx context.Context) (LLMProvider, *CircuitBreaker) {
	if s == nil || !s.IsEnabled() || s.models == nil || s.Config == nil {
		return nil, nil
	}
	host := strings.TrimSpace(s.Config.OllamaHost)
	modelName := strings.TrimSpace(s.Config.OllamaModel)
	if host == "" || modelName == "" {
		return nil, nil
	}
	ep := Endpoint{Kind: ProviderOllama, BaseURL: host, Model: modelName}
	// Nothing to fall back TO if the primary already is this local model.
	if endpointsEqual(ep, s.Config.Chat) {
		return nil, nil
	}
	// The fallback is the env-configured local Ollama model, which is not
	// necessarily on the allowlist at all, so nil means "workspace window" — the
	// same window it was built with before per-model limits existed.
	rm, err := s.models.get("fallback|"+host+"|"+modelName, ep, s.Config, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: building fallback model %s/%s failed: %v", host, modelName, err)
		return nil, nil
	}
	return rm.llm, rm.cb
}
