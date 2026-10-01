package ai

// DB-backed configuration resolution (migration 64).
//
// ResolveConfig is the single entry point used by InitAIService and
// ReloadAIService. It prefers the admin-managed configuration stored in
// Postgres (ai_settings + ai_providers) and falls back to the legacy
// environment-variable configuration when the DB is unavailable (fresh
// install before migrate, or a transient DB error at boot). This keeps
// the service bootable in every environment while making the admin panel
// the source of truth once it's populated.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// envLocalOnlyPinned reports whether AI_LOCAL_ONLY_MODE pins local-only mode on
// at the environment level. When set, the admin UI cannot turn it off.
func envLocalOnlyPinned() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_LOCAL_ONLY_MODE")))
	return v == "1" || v == "true" || v == "yes"
}

// LocalOnlyPinnedByEnv reports whether AI_LOCAL_ONLY_MODE pins local-only on
// (so the admin UI must not allow disabling it). Exported for the business layer.
func LocalOnlyPinnedByEnv() bool { return envLocalOnlyPinned() }

// splitPatterns turns the newline-delimited custom-pattern blob from the DB
// into a trimmed, non-empty slice for the redactor.
func splitPatterns(blob string) []string {
	if strings.TrimSpace(blob) == "" {
		return nil
	}
	lines := strings.Split(blob, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ResolveConfig returns the active AIConfig, preferring DB settings and
// falling back to env. Never returns nil.
func ResolveConfig(ctx context.Context) *AIConfig {
	cfg, err := BuildConfigFromDB(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: DB config unavailable, falling back to env: %v", err)
		return LoadAIConfig()
	}
	return cfg
}

// BuildConfigFromDB reads ai_settings + ai_providers and assembles a
// resolved AIConfig with normalized Chat/Embed endpoints. Errors if the
// settings row or referenced providers can't be read.
func BuildConfigFromDB(ctx context.Context) (*AIConfig, error) {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get ai settings: %w", err)
	}

	// Carry the env-derived Ollama tuning + host as the fallback substrate
	// (e.g. Anthropic chat needs an Ollama host for embeddings).
	base := LoadAIConfig()

	cfg := &AIConfig{
		Enabled:         settings.Enabled,
		RateLimitPerMin: settings.RateLimitPerMin,

		// Admin-set context window (0 → fall back to env/default via
		// EffectiveContextWindow). Flows into both the budget and num_ctx.
		ContextWindowTokens: settings.ContextWindowTokens,

		// Admin-set daily token caps (0 → fall back to env via the budget
		// resolvers). Gate all AI usage at the provider layer.
		WorkspaceDailyTokenBudget: settings.WorkspaceDailyTokenBudget,
		UserDailyTokenBudget:      settings.UserDailyTokenBudget,

		// Admin-set reasoning ("thinking") mode for capable models.
		ReasoningEnabled: settings.ReasoningEnabled,

		// Data-residency guarantee. DB setting OR an env pin forces it on; the
		// env pin can't be turned off from the admin UI (enforced in business).
		LocalOnlyMode: settings.LocalOnlyMode || envLocalOnlyPinned(),

		// PII redaction before cloud egress (independent of local-only).
		PIIRedactionEnabled: settings.PIIRedactionEnabled,
		PIICustomPatterns:   splitPatterns(settings.PIICustomPatterns),

		// Provider-agnostic web search (admin-configured). Key decrypted below.
		WebSearchProvider: settings.WebSearchProvider,
		WebSearchBaseURL:  settings.WebSearchBaseURL,
		WebSearchEnabled:  settings.WebSearchEnabled,

		// Agent execution sandbox master switch (admin-configured). Hot-reloads
		// with the rest of the config; the runner endpoint/budgets are read
		// per-run from settings by the business layer.
		SandboxEnabled: settings.SandboxEnabled,

		// Agent code-PR master switch (admin-configured). Hot-reloads with the
		// rest of the config; runner endpoint/budgets are read per-run from
		// settings by the business layer.
		CodePREnabled:           settings.CodePREnabled,
		AgentDelegationEnabled:  settings.AgentDelegationEnabled,
		AgentDelegationMaxHops:  settings.AgentDelegationMaxHops,
		AgentDelegationSurfaces: settings.AgentDelegationSurfaces,

		OllamaHost:           base.OllamaHost,
		OllamaModel:          base.OllamaModel,
		OllamaEmbeddingModel: base.OllamaEmbeddingModel,
		OllamaNumThread:      base.OllamaNumThread,
		OllamaNumCtx:         base.OllamaNumCtx,
		OllamaKeepAlive:      base.OllamaKeepAlive,
	}

	// Routing is read on its own: a failure leaves every purpose on the default,
	// which is the behaviour before routing existed, rather than failing the load.
	if routes, rerr := aiModels.GetModelRouting(ctx); rerr == nil {
		cfg.Routing = routes
	} else {
		helpers.LogErrorWithContext(ctx, "AI: model routing unreadable, using the default for every purpose: %v", rerr)
	}

	// Resolve the chat endpoint.
	chatEp, err := endpointFromProvider(ctx, settings.ChatProviderID, settings.ChatModel, 0, base)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: chat provider unresolved (%v); falling back to env chat endpoint", err)
		// Keep the admin's DB settings (budgets, windows, flags, etc.) but use
		// the env chat/embedding endpoints as a fallback so AI does not
		// silently disappear while the admin fixes the provider/key issue. The
		// env Enabled state is respected so an operator who explicitly turned
		// AI off in env is not overridden by the DB row.
		cfg.Chat = base.Chat
		cfg.Embed = base.Embed
		cfg.Enabled = base.Enabled
	} else {
		cfg.Chat = chatEp
	}

	// Resolve the embedding endpoint with its pinned dimension.
	embDim := settings.EmbeddingDimension
	if embDim <= 0 {
		embDim = 768
	}
	embEp, err := endpointFromProvider(ctx, settings.EmbeddingProviderID, settings.EmbeddingModel, embDim, base)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: embedding provider unresolved (%v); falling back to Ollama embeddings", err)
		embEp = Endpoint{Kind: ProviderOllama, BaseURL: base.OllamaHost, Model: base.OllamaEmbeddingModel, Dim: 768}
	}
	cfg.Embed = embEp

	// Resolve the OPTIONAL vision endpoint. Unset = vision disabled; any
	// resolution error degrades gracefully to "no vision" rather than
	// breaking the whole service.
	if settings.VisionProviderID != nil && settings.VisionModel != "" {
		visionEp, verr := endpointFromProvider(ctx, settings.VisionProviderID, settings.VisionModel, 0, base)
		if verr != nil {
			helpers.LogErrorWithContext(ctx, "AI: vision provider unresolved (%v); image analysis disabled", verr)
		} else {
			cfg.Vision = visionEp
		}
	}

	// Resolve the OPTIONAL dedicated code-run endpoint. Unset = code runs use the
	// chat model; any resolution error degrades gracefully to that fallback
	// rather than breaking the service.
	if settings.CodePRChatProviderID != nil && settings.CodePRChatModel != "" {
		codeRunEp, cerr := endpointFromProvider(ctx, settings.CodePRChatProviderID, settings.CodePRChatModel, 0, base)
		if cerr != nil {
			helpers.LogErrorWithContext(ctx, "AI: code-run provider unresolved (%v); code runs will use the chat model", cerr)
		} else {
			cfg.CodeRun = codeRunEp
		}
	}

	// Decrypt the web-search API key (best-effort: a decryption failure just
	// leaves the key empty, disabling cloud providers, rather than breaking
	// the whole config).
	if len(settings.WebSearchAPIKeyEnc) > 0 {
		if key, derr := aiModels.DecryptAPIKey(settings.WebSearchAPIKeyEnc); derr == nil {
			cfg.WebSearchAPIKey = key
		} else {
			helpers.LogErrorWithContext(ctx, "AI: web-search key decrypt failed: %v", derr)
		}
	}
	// Env fallback for an unset key/base-url so an operator can still configure
	// web search purely via env even with DB-backed settings present.
	if cfg.WebSearchProvider == "" {
		cfg.WebSearchProvider = base.WebSearchProvider
		cfg.WebSearchBaseURL = base.WebSearchBaseURL
		cfg.WebSearchEnabled = base.WebSearchEnabled
	}
	if cfg.WebSearchAPIKey == "" {
		cfg.WebSearchAPIKey = base.WebSearchAPIKey
	}

	return cfg, nil
}

// kind's default base URL with env fallback.
func endpointFromProvider(ctx context.Context, providerID *uuid.UUID, model string, dim int, base *AIConfig) (Endpoint, error) {
	if providerID == nil {
		return Endpoint{}, fmt.Errorf("no provider selected")
	}
	p, err := aiModels.GetProvider(ctx, *providerID)
	if err != nil {
		return Endpoint{}, err
	}
	if !p.Enabled {
		return Endpoint{}, fmt.Errorf("provider %s is disabled", p.Label)
	}
	// A STORED-BUT-UNREADABLE KEY MUST DISQUALIFY THE PROVIDER, EXPLICITLY.
	//
	// scanProvider used to return the decrypt failure, which reached here as an error and
	// produced "chat provider unresolved (...); disabling chat selection". It no longer does,
	// because that same error was failing the admin config page and locking an admin out of the
	// screen where a key is re-entered.
	//
	// So the condition has to be named here instead. Without this the provider would resolve
	// with an EMPTY APIKey and the failure would move to call time, arriving as an
	// authentication error from the provider — which reads like a wrong key rather than an
	// unreadable one, and sends the reader to the account instead of to the KEK.
	//
	// Uses the shared constructor rather than its own wording. This used to be a second,
	// hand-written sentence for the identical condition, so an admin hitting it at startup and
	// again on the admin page was told the same thing two different ways — and being a plain
	// fmt.Errorf, nothing here could be recognised with errors.Is either.
	if p.KeyUnreadable {
		return Endpoint{}, aiModels.ProviderKeyUnreadableError(p.Label)
	}

	ep := Endpoint{
		Kind:        ProviderType(p.Kind),
		BaseURL:     resolveBaseURL(p.Kind, p.BaseURL, base),
		APIKey:      p.APIKey,
		Model:       model,
		Dim:         dim,
		InsecureTLS: p.InsecureTLS,
	}
	return ep, nil
}

// resolveBaseURL applies the provider kind's default base URL when the
// stored value is empty, honoring env overrides for the built-ins so an
// operator's OLLAMA_HOST / custom OpenAI proxy still works.
func resolveBaseURL(kind, stored string, base *AIConfig) string {
	if stored != "" {
		return stored
	}
	switch kind {
	case aiModels.KindOllama:
		return base.OllamaHost // env OLLAMA_HOST or http://localhost:11434
	case aiModels.KindOpenAI:
		return "https://api.openai.com/v1"
	case aiModels.KindAnthropic:
		return "https://api.anthropic.com/v1"
	default:
		return ""
	}
}
