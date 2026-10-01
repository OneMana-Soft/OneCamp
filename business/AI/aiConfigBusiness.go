package business

// Business logic for the admin-managed, model-agnostic AI configuration.
//
// Responsibilities:
//   - Build a transient provider client from a stored ai_providers row so
//     we can list its catalog / test it WITHOUT touching the live service.
//   - List models (cached briefly in Redis) for the admin picker.
//   - Apply chat/embedding selections, then hot-reload the live service.
//   - Guard embedding-model changes against the OpenSearch dimension trap.
//   - Pull / delete local (Ollama) models with streaming progress.
//
// Everything that mutates configuration ends by calling
// ai.ReloadAIService so the change takes effect immediately for new
// requests, while in-flight requests keep their captured provider.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	codeagent "github.com/akashc777/OneCamp/business/CodeAgent"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// providerClient is the subset of behaviour the admin flows need from a
// transient (not-yet-active) provider built from a DB row.
type providerClient struct {
	kind    string
	lister  ai.ModelLister
	manager ai.ModelManager // non-nil only for Ollama
}

// ErrProviderKeyUnreadable means a provider has a stored API key that will not decrypt.
//
// AN ALIAS OF THE ONE SENTINEL, which lives in the model layer beside the KeyUnreadable flag that
// detects the condition — see aiModels.ErrProviderKeyUnreadable for why there, and
// aiModels.ProviderKeyUnreadableError for constructing it with a provider named.
//
// Re-exported here, rather than callers being pointed at the model package, because the controllers
// that need errors.Is already import this package and nothing else from models/postgres/AI. It is
// the same error VALUE, so errors.Is matches whichever name a caller reaches for.
var ErrProviderKeyUnreadable = aiModels.ErrProviderKeyUnreadable

// buildProviderClient constructs a transient client for a stored provider
// using the same resolution rules as the live loader (default base URLs,
// decrypted key). It never mutates global state.
//
// Production hardening: custom (admin-supplied) endpoints get the SSRF
// dial-time guard so they can't be pointed at cloud metadata / link-local
// addresses; built-in providers are trusted and skip it. Self-signed TLS
// is opt-in per provider via insecure_tls.
// reloadAI is the one way this package rebuilds the live service after an admin
// changes something. It also forgets the checklist's cached answer about
// whether the provider answers, because the reasons an admin reloads are the
// reasons that answer changed, and a setup step that stayed unticked for a
// minute after the fix would read as the fix not having worked.
func reloadAI(ctx context.Context) error {
	ForgetProviderProbe()
	return ai.ReloadAIService(ctx)
}

func buildProviderClient(p *aiModels.AIProvider) (*providerClient, error) {
	baseURL := resolveProviderBaseURL(p)

	// AN UNREADABLE STORED KEY MUST FAIL HERE, NOT AT THE PROVIDER. (See ErrProviderKeyUnreadable.)
	//
	// scanProvider deliberately tolerates a key it cannot decrypt so the admin AI page can load
	// and the key can be re-entered. The cost is that p.APIKey is EMPTY rather than absent, and
	// an empty key sent to an OpenAI-compatible endpoint comes back as
	//
	//	openai: models returned 401: {"error":{"message":"Invalid API Key",...}}
	//
	// which is what beta reported for a provider whose key was merely unreadable. That message
	// sends an admin to their provider account to check a key that is fine, instead of to the
	// field they need to retype. Stating the real condition here is the whole point of keeping
	// KeyUnreadable as a separate flag.
	//
	// Guarded at this chokepoint rather than in each branch below: this is the one constructor
	// the admin surfaces use (model listing, connection test), so one check covers them all.
	// Ollama is exempt because it takes no key, so an unreadable one cannot affect its client.
	if p.KeyUnreadable && p.Kind != aiModels.KindOllama {
		return nil, aiModels.ProviderKeyUnreadableError(p.Label)
	}

	// Only custom endpoints are admin-arbitrary, so only they are guarded.
	guardSSRF := p.Kind == aiModels.KindOpenAICompatible
	opts := ai.ProviderOptions{
		GuardSSRF:          guardSSRF,
		InsecureSkipVerify: p.InsecureTLS,
	}

	switch p.Kind {
	case aiModels.KindOllama:
		op := ai.NewOllamaProviderWithDims(baseURL, "", "", 0)
		return &providerClient{kind: p.Kind, lister: op, manager: op}, nil
	case aiModels.KindOpenAI:
		op := ai.NewOpenAICompatibleProviderWithOpts(p.APIKey, "", "", baseURL, 0, ai.ProviderOpenAI, opts)
		return &providerClient{kind: p.Kind, lister: op}, nil
	case aiModels.KindOpenAICompatible:
		op := ai.NewOpenAICompatibleProviderWithOpts(p.APIKey, "", "", baseURL, 0, ai.ProviderOpenAICompatible, opts)
		return &providerClient{kind: p.Kind, lister: op}, nil
	case aiModels.KindAnthropic:
		ap := ai.NewAnthropicProviderWithOpts(p.APIKey, "", baseURL, opts)
		return &providerClient{kind: p.Kind, lister: ap}, nil
	default:
		return nil, fmt.Errorf("unknown provider kind: %s", p.Kind)
	}
}

// resolveProviderBaseURL mirrors services/AI.resolveBaseURL for the
// transient-client path (the loader's version is unexported).
func resolveProviderBaseURL(p *aiModels.AIProvider) string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	switch p.Kind {
	case aiModels.KindOllama:
		return getEnvOr("OLLAMA_HOST", "http://localhost:11434")
	case aiModels.KindOpenAI:
		return "https://api.openai.com/v1"
	case aiModels.KindAnthropic:
		return "https://api.anthropic.com/v1"
	default:
		return ""
	}
}

// getEnvOr returns the env var value or a fallback when unset/empty.
func getEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// GetAIConfig assembles the full admin config payload.
func GetAIConfig(ctx context.Context) (*adapter.AIConfigResponse, error) {
	providers, err := aiModels.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return nil, err
	}

	resp := &adapter.AIConfigResponse{
		Enabled:                       settings.Enabled,
		RateLimitPerMin:               settings.RateLimitPerMin,
		ChatModel:                     settings.ChatModel,
		EmbeddingModel:                settings.EmbeddingModel,
		EmbeddingDimension:            settings.EmbeddingDimension,
		VisionModel:                   settings.VisionModel,
		ContextWindowTokens:           settings.ContextWindowTokens,
		WorkspaceDailyTokenBudget:     settings.WorkspaceDailyTokenBudget,
		UserDailyTokenBudget:          settings.UserDailyTokenBudget,
		ReasoningEnabled:              settings.ReasoningEnabled,
		MeetingRecapEnabled:           settings.MeetingRecapEnabled,
		MeetingNotesDocEnabled:        settings.MeetingNotesDocEnabled,
		MeetingRecapInstructions:      settings.MeetingRecapInstructions,
		MemoryLayerEnabled:            settings.MemoryLayerEnabled,
		TeamReportEnabled:             settings.TeamReportEnabled,
		NudgesEnabled:                 settings.NudgesEnabled,
		CoworkerEnabled:               settings.CoworkerEnabled,
		CodeAnalysisMaxFiles:          settings.CodeAnalysisMaxFiles,
		EffectiveCodeAnalysisMaxFiles: codeagent.ResolveMaxFiles(settings.CodeAnalysisMaxFiles),
		IssueTriageEnabled:            settings.IssueTriageEnabled,
		WebSearchProvider:             settings.WebSearchProvider,
		WebSearchBaseURL:              settings.WebSearchBaseURL,
		WebSearchEnabled:              settings.WebSearchEnabled,
		HasWebSearchKey:               settings.HasWebSearchKey,
		SandboxEnabled:                settings.SandboxEnabled,
		SandboxRunnerURL:              settings.SandboxRunnerURL,
		HasSandboxRunnerToken:         settings.HasSandboxRunnerToken,
		SandboxImageDigest:            settings.SandboxImageDigest,
		SandboxWorkspaceDailySeconds:  settings.SandboxWorkspaceDailySeconds,
		SandboxWorkspaceDailyRuns:     settings.SandboxWorkspaceDailyRuns,
		SandboxChannelDailySeconds:    settings.SandboxChannelDailySeconds,
		SandboxChannelDailyRuns:       settings.SandboxChannelDailyRuns,
		CodePREnabled:                 settings.CodePREnabled,
		CodePRRunnerURL:               settings.CodePRRunnerURL,
		HasCodePRRunnerToken:          settings.HasCodePRRunnerToken,
		CodePROutOfScopePolicy:        settings.CodePROutOfScopePolicy,
		CodePRDraftOnRed:              settings.CodePRDraftOnRed,
		CodePRWorkspaceDailyMinutes:   settings.CodePRWorkspaceDailyMinutes,
		CodePRWorkspaceDailyRuns:      settings.CodePRWorkspaceDailyRuns,
		CodePRChannelDailyMinutes:     settings.CodePRChannelDailyMinutes,
		CodePRChannelDailyRuns:        settings.CodePRChannelDailyRuns,
		CodePRAllowUnlinked:           settings.CodePRAllowUnlinked,
		CodePRWallMinutes:             settings.CodePRWallMinutes,
		CodePREffectiveWallMinutes:    effectiveCodingWallMinutes(settings.CodePRWallMinutes),
		CodePRChatModel:               settings.CodePRChatModel,
		LocalOnlyMode:                 settings.LocalOnlyMode || ai.LocalOnlyPinnedByEnv(),
		LocalOnlyPinnedByEnv:          ai.LocalOnlyPinnedByEnv(),
		AgentDelegationEnabled:        settings.AgentDelegationEnabled,
		AgentDelegationMaxHops:        settings.AgentDelegationMaxHops,
		AgentDelegationSurfaces:       settings.AgentDelegationSurfaces,
		AgentDelegationVetoedByEnv:    ai.DelegationVetoedByEnv(),
		PIIRedactionEnabled:           settings.PIIRedactionEnabled,
		PIICustomPatterns:             settings.PIICustomPatterns,
	}
	// Resolved window actually in force (admin value → env → 8192, floored).
	if cfg := ai.GetConfig(); cfg != nil {
		resp.EffectiveContextWindow = cfg.EffectiveContextWindow()
	}
	// Today's workspace-wide sandbox spend (best-effort; a query failure just
	// leaves the counters at zero rather than failing the whole config load).
	if u, err := aiModels.WorkspaceSandboxUsageToday(ctx); err == nil {
		resp.SandboxUsedTodaySeconds = u.Seconds
		resp.SandboxUsedTodayRuns = u.Runs
	}
	// Code-PR egress allowlist (stored as raw JSON) → []string for the FE, and
	// today's workspace-wide code-PR spend against the budgets. Both best-effort.
	if hosts := parseEgressAllowlist(settings.CodePREgressAllowlist); hosts != nil {
		resp.CodePREgressAllowlist = hosts
	}
	if u, err := aiModels.WorkspaceCodePRUsageToday(ctx); err == nil {
		resp.CodePRUsedTodayMinutes = u.Minutes
		resp.CodePRUsedTodayRuns = u.Runs
	}
	if settings.ChatProviderID != nil {
		resp.ChatProviderID = settings.ChatProviderID.String()
	}
	if settings.EmbeddingProviderID != nil {
		resp.EmbeddingProviderID = settings.EmbeddingProviderID.String()
	}
	if settings.CodePRChatProviderID != nil {
		resp.CodePRChatProviderID = settings.CodePRChatProviderID.String()
	}
	if settings.VisionProviderID != nil {
		resp.VisionProviderID = settings.VisionProviderID.String()
	}

	for _, p := range providers {
		resp.Providers = append(resp.Providers, toProviderView(p))
	}

	if svc := ai.GetService(); svc != nil && svc.Resiliency != nil {
		resp.CircuitState = svc.Resiliency.CB.State()
	}

	return resp, nil
}

// applyTestKeyOverride points a stored provider at a caller-supplied API key for the duration of a
// connection test. A blank key means "test what is stored", so it is a no-op.
//
// Extracted from TestConnection so it can be tested without a database — the branch it lives in
// begins with GetProvider, and the two properties below are the whole point of the change.
//
// CLEARS KeyUnreadable, which is the half that is easy to miss. buildProviderClient refuses to build
// anything for a provider flagged as having an undecryptable key, and it is right to: an empty key
// silently becomes a 401 from the provider that blames the admin's credential. But that guard is
// about the STORED key, and once a caller supplies one the stored key is not being used. Leaving the
// flag set means the admin can never test their way out of an unreadable key — the exact situation
// the button is needed for.
//
// Mutates the caller's provider, which is safe here because TestConnection owns that value: it came
// from GetProvider and is discarded when the probe returns. It is never written back.
func applyTestKeyOverride(p *aiModels.AIProvider, rawKey string) error {
	if p == nil || rawKey == "" {
		return nil
	}
	key, err := validateAPIKey(rawKey)
	if err != nil {
		return err
	}
	p.APIKey = key
	p.HasAPIKey = key != ""
	p.KeyUnreadable = false
	return nil
}

func toProviderView(p *aiModels.AIProvider) adapter.ProviderView {
	v := adapter.ProviderView{
		ID:            p.ID.String(),
		Kind:          p.Kind,
		Label:         p.Label,
		BaseURL:       p.BaseURL,
		HasAPIKey:     p.HasAPIKey,
		KeyUnreadable: p.KeyUnreadable,
		Enabled:       p.Enabled,
		IsBuiltin:     p.IsBuiltin,
		InsecureTLS:   p.InsecureTLS,
	}
	if !p.UpdatedAt.IsZero() {
		v.UpdatedAt = p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

// ListProviderModels returns a provider's catalog, served from a short
// Redis cache when fresh. forceRefresh bypasses the cache (used right
// after a pull/delete).
func ListProviderModels(ctx context.Context, providerID uuid.UUID, forceRefresh bool) ([]adapter.ModelView, error) {
	cacheArgs := []string{providerID.String()}

	if !forceRefresh {
		var cached []adapter.ModelView
		if found, _ := redisStore.GetJSON(ctx, registry.AIModelList, cacheArgs, &cached); found {
			return cached, nil
		}
	}

	p, err := aiModels.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	client, err := buildProviderClient(p)
	if err != nil {
		return nil, err
	}

	// Bound the upstream call so a slow/black-hole provider can't hang the
	// admin request.
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	models, err := client.lister.ListModels(listCtx)
	if err != nil {
		return nil, fmt.Errorf("%s", sanitizeProbeError(err))
	}

	views := toModelViews(models)
	// Best-effort cache; ignore write errors.
	_ = redisStore.SetJSON(ctx, registry.AIModelList, cacheArgs, views)
	return views, nil
}

func toModelViews(models []ai.ModelInfo) []adapter.ModelView {
	out := make([]adapter.ModelView, 0, len(models))
	for _, m := range models {
		out = append(out, adapter.ModelView{
			ID:        m.ID,
			Installed: m.Installed,
			SizeBytes: m.SizeBytes,
			Embedding: m.Embedding,
		})
	}
	return out
}

// invalidateModelCache drops the cached catalog for a provider.
func invalidateModelCache(ctx context.Context, providerID uuid.UUID) {
	_ = redisStore.Delete(ctx, registry.AIModelList, []string{providerID.String()})
}

// RefreshProviderModelCache invalidates and eagerly repopulates a
// provider's model cache. Called after a pull/delete so the next admin
// render reflects reality immediately.
func RefreshProviderModelCache(ctx context.Context, providerID uuid.UUID) {
	invalidateModelCache(ctx, providerID)
	if _, err := ListProviderModels(ctx, providerID, true); err != nil {
		helpers.LogErrorWithContext(ctx, "RefreshProviderModelCache: %v", err)
	}
}

// TestConnection probes a provider (stored by id, or inline values) and
// returns its catalog on success. A bounded timeout prevents a hung
// custom endpoint from blocking the request indefinitely.
func TestConnection(ctx context.Context, req adapter.TestConnectionRequest) (*adapter.TestConnectionResponse, error) {
	// Cap the probe so a black-hole endpoint can't tie up the handler.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var p *aiModels.AIProvider

	if req.ProviderID != "" {
		id, err := uuid.Parse(req.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("invalid provider_id")
		}
		p, err = aiModels.GetProvider(ctx, id)
		if err != nil {
			return nil, err
		}
		// A SUPPLIED KEY WINS OVER THE STORED ONE.
		//
		// This branch used to load the provider and never look at req.APIKey again, so the endpoint
		// accepted a field and silently ignored it — and "Test connection" on an existing provider
		// could only ever test what was already in the database.
		//
		// That is backwards for the one moment the button matters most. An admin pastes a new key
		// and presses Test to check it BEFORE committing it; testing the old key instead reports a
		// failure about a credential they are in the middle of replacing. Worse, when the stored key
		// is undecryptable it reaches the client as an empty string, so no Authorization header is
		// sent at all and the provider answers "Invalid API Key" — which reads as "the key you just
		// pasted is wrong" when the key was never sent.
		if err := applyTestKeyOverride(p, req.APIKey); err != nil {
			return &adapter.TestConnectionResponse{OK: false, Message: err.Error()}, nil
		}
	} else {
		// Inline probe (before saving a custom endpoint).
		if req.Kind == "" {
			return nil, fmt.Errorf("kind is required when provider_id is omitted")
		}
		if req.Kind != aiModels.KindOpenAICompatible {
			return nil, fmt.Errorf("inline test is only supported for custom (openai_compatible) endpoints")
		}
		baseURL, err := validateBaseURL(req.BaseURL)
		if err != nil {
			return &adapter.TestConnectionResponse{OK: false, Message: err.Error()}, nil
		}
		apiKey, err := validateAPIKey(req.APIKey)
		if err != nil {
			return &adapter.TestConnectionResponse{OK: false, Message: err.Error()}, nil
		}
		p = &aiModels.AIProvider{
			Kind:        req.Kind,
			BaseURL:     baseURL,
			APIKey:      apiKey,
			Enabled:     true,
			InsecureTLS: req.InsecureTLS,
		}
	}

	client, err := buildProviderClient(p)
	if err != nil {
		return &adapter.TestConnectionResponse{OK: false, Message: err.Error()}, nil
	}

	models, err := client.lister.ListModels(ctx)
	if err != nil {
		return &adapter.TestConnectionResponse{OK: false, Message: sanitizeProbeError(err)}, nil
	}

	return &adapter.TestConnectionResponse{
		OK:      true,
		Message: fmt.Sprintf("Connected. %d model(s) available.", len(models)),
		Models:  toModelViews(models),
	}, nil
}

// sanitizeProbeError converts low-level transport errors into safe,
// admin-friendly messages without leaking internals.
func sanitizeProbeError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "blocked for security"):
		return "Connection blocked: this address (cloud metadata / link-local) is not allowed."
	case strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "Timeout"):
		return "Connection timed out. Check the base URL is reachable from the server."
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "TLS certificate verification failed. If this is a self-signed internal endpoint, enable 'Skip TLS verification'."
	case strings.Contains(msg, "connection refused"):
		return "Connection refused. Is the endpoint running and reachable?"
	case strings.Contains(msg, "no such host"):
		return "Host not found. Check the base URL hostname."
	default:
		return msg
	}
}

// CreateCustomProvider adds a custom OpenAI-compatible endpoint after
// validating and normalizing the admin-supplied input.
func CreateCustomProvider(ctx context.Context, req adapter.CreateProviderRequest) (*adapter.ProviderView, error) {
	label, err := validateLabel(req.Label)
	if err != nil {
		return nil, err
	}
	baseURL, err := validateBaseURL(req.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey, err := validateAPIKey(req.APIKey)
	if err != nil {
		return nil, err
	}

	p, err := aiModels.CreateCustomProvider(ctx, label, baseURL, apiKey, req.InsecureTLS)
	if err != nil {
		return nil, err
	}
	v := toProviderView(p)
	return &v, nil
}

// UpdateProvider edits a provider and reloads the service if the edited
// provider is currently active. Validates any supplied fields.
func UpdateProvider(ctx context.Context, id uuid.UUID, req adapter.UpdateProviderRequest) (*adapter.ProviderView, error) {
	in := aiModels.UpdateProviderInput{
		Enabled:     req.Enabled,
		InsecureTLS: req.InsecureTLS,
	}

	if req.Label != nil {
		l, err := validateLabel(*req.Label)
		if err != nil {
			return nil, err
		}
		in.Label = &l
	}
	if req.BaseURL != nil {
		// Only custom endpoints have an editable base URL. Validate it.
		existing, err := aiModels.GetProvider(ctx, id)
		if err != nil {
			return nil, err
		}
		if existing.Kind != aiModels.KindOpenAICompatible {
			return nil, fmt.Errorf("base_url can only be changed for custom endpoints")
		}
		b, err := validateBaseURL(*req.BaseURL)
		if err != nil {
			return nil, err
		}
		in.BaseURL = &b
	}
	if req.APIKey != nil {
		k, err := validateAPIKey(*req.APIKey)
		if err != nil {
			return nil, err
		}
		in.APIKey = &k
	}

	p, err := aiModels.UpdateProvider(ctx, id, in)
	if err != nil {
		return nil, err
	}
	invalidateModelCache(ctx, id)

	// If the edited provider is the active chat or embedding provider,
	// rebuild the live service so credential/URL changes take effect.
	if err := reloadIfActive(ctx, id); err != nil {
		helpers.LogErrorWithContext(ctx, "UpdateProvider reload: %v", err)
	}

	v := toProviderView(p)
	return &v, nil
}

// DeleteCustomProvider removes a custom provider unless it's active.
func DeleteCustomProvider(ctx context.Context, id uuid.UUID) error {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return err
	}
	if (settings.ChatProviderID != nil && *settings.ChatProviderID == id) ||
		(settings.EmbeddingProviderID != nil && *settings.EmbeddingProviderID == id) {
		return fmt.Errorf("cannot delete a provider that is currently in use; switch the active model first")
	}
	if err := aiModels.DeleteCustomProvider(ctx, id); err != nil {
		return err
	}
	invalidateModelCache(ctx, id)
	return nil
}

// guardLocalOnlySelection rejects activating a non-local (cloud) provider while
// local-only AI mode is on, so the guarantee can't be broken by a model swap
// (Requirement 1.3). The dial guard is the runtime backstop; this gives the
// admin a clear, fast config-time error instead.
func guardLocalOnlySelection(p *aiModels.AIProvider) error {
	cfg := ai.GetConfig()
	if cfg == nil || !cfg.LocalOnlyMode {
		return nil
	}
	if ai.EndpointCloudCheck(ai.ProviderType(p.Kind), p.BaseURL) {
		return fmt.Errorf("local-only AI mode is on: %q is not a local provider. Choose a local model (Ollama or a private endpoint) or disable local-only mode first", p.Label)
	}
	return nil
}

// SetChatModel sets the active chat selection and hot-reloads.
func SetChatModel(ctx context.Context, req adapter.SetChatModelRequest) error {
	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider_id")
	}
	model, err := validateModelName(req.Model)
	if err != nil {
		return err
	}
	// Validate the provider exists & is enabled.
	p, err := aiModels.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("provider %s is disabled", p.Label)
	}
	if err := guardLocalOnlySelection(p); err != nil {
		return err
	}
	// Best-effort: verify the model exists in the provider's catalog so an
	// admin can't activate a typo that would 404 every AI call. If the
	// catalog can't be enumerated (endpoint without /models, transient
	// outage) we allow the selection — the admin may legitimately know a
	// valid id the picker can't list.
	if err := validateModelInCatalog(ctx, p, model, false); err != nil {
		return err
	}
	if err := aiModels.SetChatSelection(ctx, id, model); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetVisionModel sets or clears the optional vision (multimodal) selection and
// hot-reloads. An empty provider_id or model CLEARS it (image analysis off).
func SetVisionModel(ctx context.Context, req adapter.SetVisionModelRequest) error {
	// Clear path: empty provider or model disables vision.
	if req.ProviderID == "" || req.Model == "" {
		if err := aiModels.SetVisionSelection(ctx, nil, ""); err != nil {
			return err
		}
		return reloadAI(ctx)
	}

	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider_id")
	}
	model, err := validateModelName(req.Model)
	if err != nil {
		return err
	}
	p, err := aiModels.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("provider %s is disabled", p.Label)
	}
	if err := guardLocalOnlySelection(p); err != nil {
		return err
	}
	// Best-effort catalog check (same posture as chat): a valid manual id is
	// allowed when the catalog can't be enumerated.
	if err := validateModelInCatalog(ctx, p, model, false); err != nil {
		return err
	}
	if err := aiModels.SetVisionSelection(ctx, &id, model); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetCodePRModel sets or clears the optional dedicated model the code-PR coding
// runner uses (so code runs don't compete with the chat model's provider quota),
// then hot-reloads. An empty provider_id or model CLEARS it (code runs fall back
// to the chat model). Mirrors SetVisionModel's validation.
func SetCodePRModel(ctx context.Context, req adapter.SetCodePRModelRequest) error {
	if req.ProviderID == "" || req.Model == "" {
		if err := aiModels.SetCodePRModelSelection(ctx, nil, ""); err != nil {
			return err
		}
		return reloadAI(ctx)
	}

	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider_id")
	}
	model, err := validateModelName(req.Model)
	if err != nil {
		return err
	}
	p, err := aiModels.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("provider %s is disabled", p.Label)
	}
	if err := guardLocalOnlySelection(p); err != nil {
		return err
	}
	if err := validateModelInCatalog(ctx, p, model, false); err != nil {
		return err
	}
	if err := aiModels.SetCodePRModelSelection(ctx, &id, model); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// validateModelInCatalog checks that model is present in the provider's
// catalog. It is best-effort: a catalog-fetch failure is NOT fatal (returns
// nil) because some valid endpoints don't implement /models and the admin may
// type a known-good id. When the catalog IS available and the model is absent,
// it returns a clear error. wantEmbedding narrows the "did you mean" hint.
func validateModelInCatalog(ctx context.Context, p *aiModels.AIProvider, model string, wantEmbedding bool) error {
	client, err := buildProviderClient(p)
	if err != nil {
		return err
	}
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	models, err := client.lister.ListModels(listCtx)
	if err != nil {
		// Catalog unavailable — don't block a possibly-valid manual id.
		helpers.LogInfoWithContext(ctx,
			"AI: could not verify model %q against %s catalog (allowing): %v", model, p.Label, err)
		return nil
	}
	if len(models) == 0 {
		// Empty catalog (e.g. Ollama with nothing pulled yet) — don't block.
		return nil
	}
	for _, m := range models {
		if m.ID == model {
			return nil
		}
	}
	kind := "chat"
	if wantEmbedding {
		kind = "embedding"
	}
	return fmt.Errorf("%s model %q is not available on provider %s; pick one from its model list", kind, model, p.Label)
}

// SetEmbeddingModel sets the active embedding selection. Changing the
// dimension is the dangerous path: the OpenSearch k-NN index is pinned to
// one dimension, so a mismatch silently breaks search. We:
//   - probe the model's ACTUAL output dimension (never trust the client),
//   - refuse a dimension change unless the caller authorizes a reindex,
//   - buffer live writes across the rebuild so concurrent edits aren't lost.
func SetEmbeddingModel(ctx context.Context, req adapter.SetEmbeddingModelRequest) error {
	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider_id")
	}
	model, err := validateModelName(req.Model)
	if err != nil {
		return err
	}
	if req.Dimension <= 0 || req.Dimension > 65536 {
		return fmt.Errorf("dimension must be between 1 and 65536")
	}

	p, err := aiModels.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("provider %s is disabled", p.Label)
	}
	if err := guardLocalOnlySelection(p); err != nil {
		return err
	}

	current, err := aiModels.GetSettings(ctx)
	if err != nil {
		return err
	}

	// Probe the model's REAL output dimension via a transient embedder so we
	// never pin the index to a client-asserted value that doesn't match what
	// the model emits (which would silently drop every future embedding).
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	measuredDim, probeErr := probeEmbeddingDimension(probeCtx, p, model)
	if probeErr != nil {
		return fmt.Errorf("could not verify embedding model %q: %s", model, sanitizeProbeError(probeErr))
	}
	if measuredDim != req.Dimension {
		// The measured value is authoritative; the index must match what the
		// model produces. Log the correction so the admin can see it.
		helpers.LogInfoWithContext(ctx,
			"AI: embedding model %q produces %d-dim vectors (client claimed %d); using %d",
			model, measuredDim, req.Dimension, measuredDim)
	}
	dimension := measuredDim

	dimensionChanged := dimension != current.EmbeddingDimension
	if dimensionChanged && !req.Reindex {
		return fmt.Errorf(
			"changing embedding dimension from %d to %d requires reindexing all content; "+
				"re-submit with reindex=true to authorize (existing AI search will be rebuilt)",
			current.EmbeddingDimension, dimension)
	}

	// Refuse to start a second reindex on top of a running one — that would
	// corrupt the in-flight rebuild.
	if dimensionChanged && ai.IsReindexRunning() {
		return fmt.Errorf("an embedding reindex is already in progress; wait for it to finish before changing the dimension again")
	}

	// Arm write-buffering BEFORE swapping the live embedder, so no live write
	// slips through at the new dimension while the index is still old-dim.
	if dimensionChanged {
		ai.BeginReindexBuffering()
	}

	if err := aiModels.SetEmbeddingSelection(ctx, id, model, dimension); err != nil {
		if dimensionChanged {
			ai.AbortReindexBuffering()
		}
		return err
	}

	// Reload so the new embedder is live before the reindex runs.
	if err := reloadAI(ctx); err != nil {
		if dimensionChanged {
			ai.AbortReindexBuffering()
		}
		return err
	}

	if dimensionChanged {
		// Recreate the OpenSearch index at the new dimension and re-embed
		// everything. Heavy background job; the admin monitors via reindex
		// status. Buffered live writes are replayed at the end.
		helpers.LogInfoWithContext(ctx,
			"AI embedding dimension changed %d→%d; starting reindex", current.EmbeddingDimension, dimension)
		if !ai.StartEmbeddingReindexAsync(dimension) {
			// Lost the start race (a reindex began between our check and
			// here) — tear down buffering so live writes resume direct.
			ai.AbortReindexBuffering()
			return fmt.Errorf("an embedding reindex is already in progress; the model was updated but the index rebuild was not started")
		}
	}

	return nil
}

// probeEmbeddingDimension builds a transient endpoint for the provider+model
// and measures the real embedding dimension without touching live state.
func probeEmbeddingDimension(ctx context.Context, p *aiModels.AIProvider, model string) (int, error) {
	ep := ai.Endpoint{
		Kind:        ai.ProviderType(p.Kind),
		BaseURL:     resolveProviderBaseURL(p),
		APIKey:      p.APIKey,
		Model:       model,
		InsecureTLS: p.InsecureTLS,
	}
	// Pass the live config so an Anthropic-embed selection (which has no
	// native embeddings) probes its real Ollama fallback dimension.
	cfg := ai.GetConfig()
	return ai.ProbeEmbeddingDimension(ctx, ep, cfg)
}

// SetEnabled toggles AI globally and reloads.
func SetEnabled(ctx context.Context, enabled bool) error {
	if err := aiModels.SetEnabled(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetRateLimit updates the per-user ceiling and reloads.
func SetRateLimit(ctx context.Context, perMin int) error {
	if perMin <= 0 || perMin > 10000 {
		return fmt.Errorf("rate_limit_per_min must be between 1 and 10000")
	}
	if err := aiModels.SetRateLimit(ctx, perMin); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetContextWindow updates the model context window (tokens) and reloads the
// service so BOTH the prompt token budget and the provider's num_ctx pick up
// the new value together. 0 means "use the env/default". A non-zero value is
// bounded: at least 2048 (below which prompts can't fit) and at most 1,000,000
// (generous ceiling for large cloud windows; guards against fat-finger input
// that would make the budget effectively unbounded).
func SetContextWindow(ctx context.Context, tokens int) error {
	if tokens != 0 {
		if tokens < 2048 {
			return fmt.Errorf("context_window_tokens must be 0 (default) or at least 2048")
		}
		if tokens > 1_000_000 {
			return fmt.Errorf("context_window_tokens must be at most 1000000")
		}
	}
	if err := aiModels.SetContextWindowTokens(ctx, tokens); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// budgetCeiling guards the daily token caps against fat-finger input that would
// be meaningless. 1 billion tokens/day is far above any realistic workspace.
const budgetCeiling = 1_000_000_000

// SetWorkspaceDailyTokenBudget sets the workspace-wide daily AI token cap and
// reloads so the new value gates the next request. 0 = unlimited.
func SetWorkspaceDailyTokenBudget(ctx context.Context, tokens int) error {
	if tokens < 0 || tokens > budgetCeiling {
		return fmt.Errorf("workspace_daily_token_budget must be between 0 (unlimited) and %d", budgetCeiling)
	}
	if err := aiModels.SetWorkspaceDailyTokenBudget(ctx, tokens); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetUserDailyTokenBudget sets the per-user daily AI token cap and reloads so
// the new value gates the next request. 0 = unlimited.
func SetUserDailyTokenBudget(ctx context.Context, tokens int) error {
	if tokens < 0 || tokens > budgetCeiling {
		return fmt.Errorf("user_daily_token_budget must be between 0 (unlimited) and %d", budgetCeiling)
	}
	if err := aiModels.SetUserDailyTokenBudget(ctx, tokens); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetReasoning toggles "thinking" mode for reasoning-capable models. Reloads
// the service so the provider (which captures the flag at construction) picks
// it up immediately for new requests.
func SetReasoning(ctx context.Context, enabled bool) error {
	if err := aiModels.SetReasoningEnabled(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetLocalOnly toggles the data-residency guarantee. Enabling is refused while
// any active model endpoint (chat/embeddings/vision) is non-local, so an admin
// gets a clear message instead of silently-broken AI. An env pin
// (AI_LOCAL_ONLY_MODE) cannot be turned off from here. Reloads the service so
// the dial guard takes effect immediately.
func SetLocalOnly(ctx context.Context, enabled bool) error {
	if !enabled && ai.LocalOnlyPinnedByEnv() {
		return fmt.Errorf("local-only mode is pinned on by AI_LOCAL_ONLY_MODE and cannot be disabled here")
	}
	if enabled {
		if cfg := ai.GetConfig(); cfg != nil {
			if bad := cfg.NonLocalActiveEndpoints(); len(bad) > 0 {
				return fmt.Errorf("cannot enable local-only mode: these active model endpoints are not local: %s. Switch them to a local provider first", strings.Join(bad, ", "))
			}
		}
	}
	if err := aiModels.SetLocalOnlyMode(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetPIIRedaction toggles PII redaction before cloud egress. Reloads the
// service so the per-provider redactor is (re)built at construction.
func SetPIIRedaction(ctx context.Context, enabled bool) error {
	if err := aiModels.SetPIIRedactionEnabled(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetPIICustomPatterns stores the admin-defined redaction regexes (one per
// line). Every non-empty line is compile-checked first so the admin gets
// immediate feedback; on success the service reloads to rebuild the redactor.
func SetPIICustomPatterns(ctx context.Context, patterns string) error {
	var bad []string
	for i, line := range strings.Split(patterns, "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if _, err := regexp.Compile(p); err != nil {
			bad = append(bad, fmt.Sprintf("line %d", i+1))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("invalid regex on %s", strings.Join(bad, ", "))
	}
	if err := aiModels.SetPIICustomPatterns(ctx, patterns); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetMeetingRecapEnabled toggles the post-call recap ambient agent. No
// service reload needed — the agent reads the setting live on each call.
func SetMeetingRecapEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetMeetingRecapEnabled(ctx, enabled)
}

// SetMeetingNotesDocEnabled toggles writing the recap and transcript into a
// document as well as posting it. Read live by the recap agent on each call, so
// no service reload is needed.
func SetMeetingNotesDocEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetMeetingNotesDocEnabled(ctx, enabled)
}

// maxRecapInstructionsLen bounds the stored custom recap guidance so it can't
// blow the recap prompt budget or become an injection vector.
const maxRecapInstructionsLen = 2000

// SetMeetingRecapInstructions stores optional admin guidance appended to the
// recap prompt. Trimmed and length-capped; read live by the recap agent on each
// call, so no service reload is needed.
func SetMeetingRecapInstructions(ctx context.Context, instructions string) error {
	instructions = strings.TrimSpace(instructions)
	if len(instructions) > maxRecapInstructionsLen {
		instructions = instructions[:maxRecapInstructionsLen]
	}
	return aiModels.SetMeetingRecapInstructions(ctx, instructions)
}

// SetCoworkerEnabled toggles the @mention AI coworker. No service reload
// needed — the coworker reads the setting live when it is mentioned.
func SetCoworkerEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetCoworkerEnabled(ctx, enabled)
}

// validWebSearchProviders is the provider-agnostic allow-list. An empty
// provider disables the tool.
var validWebSearchProviders = map[string]bool{"": true, "searxng": true, "tavily": true, "brave": true}

// SetWebSearch configures the provider-agnostic web search. The API key is
// encrypted before storage and only updated when (re)entered or cleared.
// Reloads the service so the resolved config (and the tool's availability)
// takes effect immediately.
func SetWebSearch(ctx context.Context, req adapter.SetWebSearchRequest) error {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if !validWebSearchProviders[provider] {
		return fmt.Errorf("unsupported web search provider %q (use searxng, tavily, or brave)", req.Provider)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if provider == "searxng" && req.Enabled && baseURL == "" {
		return fmt.Errorf("a base URL is required for the searxng provider")
	}

	var encKey []byte
	updateKey := false
	switch {
	case req.ClearKey:
		updateKey = true // write NULL
	case strings.TrimSpace(req.APIKey) != "":
		enc, err := aiModels.EncryptAPIKey(strings.TrimSpace(req.APIKey))
		if err != nil {
			return fmt.Errorf("failed to secure the API key")
		}
		encKey = enc
		updateKey = true
	}

	if err := aiModels.SetWebSearch(ctx, provider, baseURL, req.Enabled, encKey, updateKey); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// Safety ceilings for the sandbox daily budgets. Even an admin can't set an
// effectively-unbounded budget by fat-fingering a huge number: seconds are
// capped at 24h of runner time and runs at 100k/day per tier. 0 means
// "unlimited" and is preserved as-is (the sandbox budget check treats 0 as no
// cap), so these ceilings only bound explicit positive values.
const (
	maxSandboxDailySeconds = 24 * 60 * 60 // 86400s of accumulated runner time
	maxSandboxDailyRuns    = 100_000
)

// clampSandboxBudget bounds a single daily-budget value: negatives become 0
// (unlimited), and positive values are capped at max. Pure and total.
func clampSandboxBudget(v, max int) int {
	if v <= 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// SetSandboxConfig configures the agent execution sandbox: the isolated
// code-runner sidecar URL, its auth token (encrypted before storage, updated
// only when re-entered or cleared), the pinned image digest, and the
// workspace/channel daily budgets. Validates the runner URL and clamps the
// budgets to safe bounds, then hot-reloads the service so the tool's
// availability takes effect immediately.
func SetSandboxConfig(ctx context.Context, req adapter.SetSandboxConfigRequest) error {
	runnerURL := strings.TrimRight(strings.TrimSpace(req.RunnerURL), "/")
	if req.Enabled && runnerURL == "" {
		return fmt.Errorf("a runner URL is required to enable the sandbox")
	}
	if runnerURL != "" {
		u, err := url.Parse(runnerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("runner URL must be an absolute http(s) URL")
		}
	}
	imageDigest := strings.TrimSpace(req.ImageDigest)

	wsSeconds := clampSandboxBudget(req.WorkspaceDailySeconds, maxSandboxDailySeconds)
	wsRuns := clampSandboxBudget(req.WorkspaceDailyRuns, maxSandboxDailyRuns)
	chSeconds := clampSandboxBudget(req.ChannelDailySeconds, maxSandboxDailySeconds)
	chRuns := clampSandboxBudget(req.ChannelDailyRuns, maxSandboxDailyRuns)

	var encToken []byte
	updateToken := false
	switch {
	case req.ClearToken:
		updateToken = true // write NULL
	case strings.TrimSpace(req.RunnerToken) != "":
		enc, err := aiModels.EncryptAPIKey(strings.TrimSpace(req.RunnerToken))
		if err != nil {
			return fmt.Errorf("failed to secure the runner token")
		}
		encToken = enc
		updateToken = true
	}

	if err := aiModels.SetSandboxConfig(ctx, req.Enabled, runnerURL, imageDigest,
		wsSeconds, wsRuns, chSeconds, chRuns, encToken, updateToken); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetSandboxEnabled is the instant kill switch for the execution sandbox:
// toggles only the master enable flag (runner config and budgets untouched) and
// hot-reloads the service so the tool becomes (un)available immediately.
func SetSandboxEnabled(ctx context.Context, enabled bool) error {
	if err := aiModels.SetSandboxEnabled(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// Safety ceilings for the code-PR daily budgets. 0 = unlimited (preserved).
const (
	maxCodePRDailyMinutes = 24 * 60 // a full day of runner wall-clock per tier
	maxCodePRDailyRuns    = 10_000
)

// Valid out-of-scope policies (matches the migration default + design).
const (
	codePRPolicyFlagOpen = "flag_open" // open the PR flagged with the scope concern (default)
	codePRPolicyPause    = "pause"     // pause via needs_human on scope drift
)

// effectiveCodingWallMinutes resolves the coding wall limit actually in force
// for a stored setting (0 = the built-in default / env override) and returns it
// in minutes, so the admin sees the real limit rather than a raw 0. Resolving it
// also REFRESHES the coding layer's cached wall from the row we just read, which
// is why every settings load keeps the derived deadlines (lease TTL, transport
// ceiling) tracking the stored value without a DB read on the timing path.
func effectiveCodingWallMinutes(settingMinutes int) int {
	return int(codepr.SetConfiguredCodingWall(settingMinutes) / time.Minute)
}

// clampCodePRBudget bounds a single daily-budget value: negatives become 0
// (unlimited), positive values are capped at max. Pure and total.
func clampCodePRBudget(v, max int) int {
	if v <= 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// hostRe validates an egress allowlist entry as a bare host[:port] (no scheme,
// no path, no spaces) so the runner's network policy gets clean host tokens.
var hostRe = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$`)

// normalizeEgressAllowlist trims, lowercases, de-dupes and validates the host
// list, returning a compact JSON array. A malformed entry is rejected so a
// typo can't silently widen (or void) the runner's egress policy.
func normalizeEgressAllowlist(hosts []string) (string, error) {
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if !hostRe.MatchString(h) {
			return "", fmt.Errorf("invalid egress host %q (use a bare host like api.github.com)", h)
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode egress allowlist: %w", err)
	}
	return string(b), nil
}

// parseEgressAllowlist decodes the stored raw-JSON host array into a slice for
// the FE. Returns an empty non-nil slice on empty/invalid input so the FE always
// gets an array, never null.
func parseEgressAllowlist(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return []string{}
	}
	return hosts
}

// SetCodePRConfig configures the agent code-PR feature: the coding-capable
// code-runner sidecar URL, its auth token (encrypted; updated only when
// re-entered or cleared), the egress allowlist, the out-of-scope policy, the
// draft-on-red flag, and the workspace/channel daily budgets (minutes + runs).
// Validates the runner URL + egress hosts + policy and clamps the budgets, then
// hot-reloads the service so the tool's availability takes effect immediately.
func SetCodePRConfig(ctx context.Context, req adapter.SetCodePRConfigRequest) error {
	runnerURL := strings.TrimRight(strings.TrimSpace(req.RunnerURL), "/")
	if req.Enabled && runnerURL == "" {
		return fmt.Errorf("a runner URL is required to enable code PRs")
	}
	if runnerURL != "" {
		u, err := url.Parse(runnerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("runner URL must be an absolute http(s) URL")
		}
	}

	egressJSON, err := normalizeEgressAllowlist(req.EgressAllowlist)
	if err != nil {
		return err
	}

	policy := strings.TrimSpace(req.OutOfScopePolicy)
	switch policy {
	case codePRPolicyFlagOpen, codePRPolicyPause:
		// ok
	case "":
		policy = codePRPolicyFlagOpen
	default:
		return fmt.Errorf("out-of-scope policy must be %q or %q", codePRPolicyFlagOpen, codePRPolicyPause)
	}

	// How long ONE coding run may work. 0 = use the built-in default; anything
	// else must land inside the range the coding layer enforces, and we say so
	// instead of silently clamping a typo into a limit the admin didn't choose.
	wallMinutes := req.WallMinutes
	if wallMinutes < 0 || (wallMinutes > 0 &&
		(wallMinutes < codepr.MinCodingWallMinutes || wallMinutes > codepr.MaxCodingWallMinutes)) {
		return fmt.Errorf("coding time limit must be 0 (use the default) or between %d and %d minutes",
			codepr.MinCodingWallMinutes, codepr.MaxCodingWallMinutes)
	}

	wsMinutes := clampCodePRBudget(req.WorkspaceDailyMinutes, maxCodePRDailyMinutes)
	wsRuns := clampCodePRBudget(req.WorkspaceDailyRuns, maxCodePRDailyRuns)
	chMinutes := clampCodePRBudget(req.ChannelDailyMinutes, maxCodePRDailyMinutes)
	chRuns := clampCodePRBudget(req.ChannelDailyRuns, maxCodePRDailyRuns)

	var encToken []byte
	updateToken := false
	switch {
	case req.ClearToken:
		updateToken = true // write NULL
	case strings.TrimSpace(req.RunnerToken) != "":
		enc, encErr := aiModels.EncryptAPIKey(strings.TrimSpace(req.RunnerToken))
		if encErr != nil {
			return fmt.Errorf("failed to secure the runner token")
		}
		encToken = enc
		updateToken = true
	}

	if err := aiModels.SetCodePRConfig(ctx, req.Enabled, runnerURL, egressJSON, policy, req.DraftOnRed, req.AllowUnlinked,
		codepr.ClampCodingWallMinutes(wallMinutes), wsMinutes, wsRuns, chMinutes, chRuns, encToken, updateToken); err != nil {
		return err
	}
	// Publish the new wall limit to the coding layer's cache so the runs that
	// follow — and every deadline derived from the limit (the durable queue's
	// lease TTL, the sidecar transport ceiling) — use it without a DB read.
	codepr.SetConfiguredCodingWall(wallMinutes)
	return reloadAI(ctx)
}

// SetCodePREnabled is the instant kill switch for the code-PR agent: toggles
// only the master enable flag (runner config + budgets untouched) and hot-
// reloads so the tool becomes (un)available immediately.
func SetCodePREnabled(ctx context.Context, enabled bool) error {
	if err := aiModels.SetCodePREnabled(ctx, enabled); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// SetCodeAnalysisMaxFiles sets the per-analysis file budget for the code-aware
// bug agent, clamped to the agent's safe range (0 = use the default). Read live
// by the agent on each run, so no service reload is needed.
func SetCodeAnalysisMaxFiles(ctx context.Context, n int) error {
	if n != 0 {
		n = codeagent.ResolveMaxFiles(n)
	}
	return aiModels.SetCodeAnalysisMaxFiles(ctx, n)
}

// SetIssueTriageEnabled toggles opt-in auto-analysis of newly-opened GitHub
// issues. Read live by the triage listener, so no service reload is needed.
func SetIssueTriageEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetIssueTriageEnabled(ctx, enabled)
}

// DeleteModel removes a locally-installed model from a provider that
// supports management (Ollama).
func DeleteModel(ctx context.Context, providerID uuid.UUID, model string) error {
	model, err := validateModelName(model)
	if err != nil {
		return err
	}
	p, err := aiModels.GetProvider(ctx, providerID)
	if err != nil {
		return err
	}
	client, err := buildProviderClient(p)
	if err != nil {
		return err
	}
	if client.manager == nil {
		return fmt.Errorf("provider %s does not support deleting models", p.Label)
	}
	// Refuse to delete the currently-active model out from under the
	// running service — that would break chat/embeddings immediately.
	if err := guardActiveModelNotDeleted(ctx, providerID, model); err != nil {
		return err
	}
	if err := client.manager.DeleteModel(ctx, model); err != nil {
		return err
	}
	invalidateModelCache(ctx, providerID)
	return nil
}

// guardActiveModelNotDeleted returns an error if (providerID, model) is
// the active chat or embedding selection.
func guardActiveModelNotDeleted(ctx context.Context, providerID uuid.UUID, model string) error {
	s, err := aiModels.GetSettings(ctx)
	if err != nil {
		return err
	}
	if s.ChatProviderID != nil && *s.ChatProviderID == providerID && s.ChatModel == model {
		return fmt.Errorf("cannot delete %q: it is the active chat model; switch the chat model first", model)
	}
	if s.EmbeddingProviderID != nil && *s.EmbeddingProviderID == providerID && s.EmbeddingModel == model {
		return fmt.Errorf("cannot delete %q: it is the active embedding model; switch the embedding model first", model)
	}
	return nil
}

// ValidatePullModelName validates and normalizes a model tag for a pull
// request (exported for the streaming controller, which validates before
// opening the SSE stream).
func ValidatePullModelName(model string) (string, error) {
	return validateModelName(model)
}

// GetManagerForPull resolves a provider to its model manager for the
// streaming pull controller. Returns an error if the provider can't
// install models.
func GetManagerForPull(ctx context.Context, providerID uuid.UUID) (ai.ModelManager, *aiModels.AIProvider, error) {
	p, err := aiModels.GetProvider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	client, err := buildProviderClient(p)
	if err != nil {
		return nil, nil, err
	}
	if client.manager == nil {
		return nil, nil, fmt.Errorf("provider %s does not support installing models", p.Label)
	}
	return client.manager, p, nil
}

// GetOllamaCatalog returns the curated, installable Ollama model catalog
// annotated with (a) which models are already installed on this provider and
// (b) how well each fits the server's actual RAM/disk. This powers the admin
// "browse & install" experience.
//
// The provider must be a local Ollama provider (the only kind that installs
// models). Installed state is read live (best-effort: a catalog fetch failure
// degrades to "no install annotations" rather than failing the whole call, so
// the admin can still browse and install).
func GetOllamaCatalog(ctx context.Context, providerID uuid.UUID) (*adapter.OllamaCatalogResponse, error) {
	p, err := aiModels.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if p.Kind != aiModels.KindOllama {
		return nil, fmt.Errorf("the model catalog is only available for local Ollama providers")
	}

	// Live installed set (best-effort, cached). Map by tag for O(1) lookup.
	installed := map[string]bool{}
	if views, err := ListProviderModels(ctx, providerID, false); err == nil {
		for _, m := range views {
			installed[m.ID] = true
		}
	} else {
		helpers.LogInfoWithContext(ctx, "GetOllamaCatalog: installed-models lookup failed (continuing): %v", err)
	}

	// Server resources for feasibility hints (best-effort).
	stats := ai.GetSystemStats(ctx)

	catalog := ai.OllamaCatalog()
	out := make([]adapter.CatalogModelView, 0, len(catalog))
	for _, m := range catalog {
		v := adapter.CatalogModelView{
			Tag:          m.Tag,
			Family:       m.Family,
			DisplayName:  m.DisplayName,
			Description:  m.Description,
			Parameters:   m.Parameters,
			SizeBytes:    m.SizeBytes,
			MinRAMBytes:  m.MinRAMBytes,
			Capabilities: capsToStrings(m.Capabilities),
			Recommended:  m.Recommended,
			Installed:    installed[m.Tag],
		}
		v.Fit, v.FitReason = modelFit(m, stats)
		out = append(out, v)
	}

	return &adapter.OllamaCatalogResponse{ProviderID: providerID.String(), Models: out}, nil
}

// RefreshOllamaCatalog forces a remote-manifest re-fetch (bypassing the cache),
// then returns the freshly-merged, annotated catalog for the provider. Used by
// the admin "refresh" action so a newly-published manifest is picked up on
// demand rather than waiting for the cache TTL. A remote fetch error is
// surfaced to the admin but the (embedded/last-known) catalog is still
// returned so the browser keeps working.
func RefreshOllamaCatalog(ctx context.Context, providerID uuid.UUID) (*adapter.OllamaCatalogResponse, error) {
	p, err := aiModels.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if p.Kind != aiModels.KindOllama {
		return nil, fmt.Errorf("the model catalog is only available for local Ollama providers")
	}

	// Force the remote re-fetch; log (don't fail) on error.
	if _, err := ai.RefreshOllamaCatalog(ctx); err != nil {
		helpers.LogInfoWithContext(ctx, "RefreshOllamaCatalog: remote manifest refresh failed (serving baseline): %v", err)
	}

	// Re-fetch installed state too (force-refresh) so the browser is current.
	if _, err := ListProviderModels(ctx, providerID, true); err != nil {
		helpers.LogInfoWithContext(ctx, "RefreshOllamaCatalog: installed-models refresh failed (continuing): %v", err)
	}

	return GetOllamaCatalog(ctx, providerID)
}

// capsToStrings converts typed capability tags to plain strings for the DTO.
func capsToStrings(caps []ai.CatalogCapability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

// modelFit grades how well a model fits the server's resources. It is a UX
// hint, not a hard gate — the admin can install anything. Returns ("", "")
// when stats are unavailable so the UI simply omits the hint.
func modelFit(m ai.CatalogModel, stats *ai.SystemStats) (string, string) {
	if stats == nil || stats.MemTotalBytes == 0 {
		return "", ""
	}
	total := int64(stats.MemTotalBytes)

	// Disk: must have room to download the weights (with a little slack).
	if stats.DiskFreeBytes > 0 && m.SizeBytes > 0 {
		needed := m.SizeBytes + (m.SizeBytes / 10) // +10% slack for temp/unpack
		if int64(stats.DiskFreeBytes) < needed {
			return "risky", "Not enough free disk to download this model."
		}
	}

	// RAM: compare recommended RAM against total system memory.
	if m.MinRAMBytes > 0 {
		switch {
		case total < m.MinRAMBytes:
			return "risky", "Your server has less RAM than this model recommends; it may fail to load or run very slowly."
		case total < m.MinRAMBytes*5/4: // within 25% of the recommendation
			return "tight", "This model is close to your server's RAM limit; expect slower responses."
		}
	}
	return "ok", ""
}

// GetSystemStats returns server resource info plus Ollama version
// awareness when a local Ollama provider exists.
func GetSystemStats(ctx context.Context) *adapter.SystemStatsResponse {
	stats := ai.GetSystemStats(ctx)
	resp := &adapter.SystemStatsResponse{
		DiskPath:        stats.DiskPath,
		DiskTotalBytes:  stats.DiskTotalBytes,
		DiskFreeBytes:   stats.DiskFreeBytes,
		DiskUsedPercent: stats.DiskUsedPercent,
		MemTotalBytes:   stats.MemTotalBytes,
		MemFreeBytes:    stats.MemFreeBytes,
		MemUsedPercent:  stats.MemUsedPercent,
		CPUCount:        stats.CPUCount,
		CPUUsedPercent:  stats.CPUUsedPct,
		Warnings:        stats.Warnings,
	}

	// Best-effort Ollama version awareness.
	if p := findOllamaProvider(ctx); p != nil {
		if client, err := buildProviderClient(p); err == nil && client.manager != nil {
			// Bound the version probe so a slow Ollama can't stall the panel.
			vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if ver, err := client.manager.Version(vctx); err == nil {
				resp.OllamaVersion = ver
				if latest := cachedLatestOllamaVersion(ctx); latest != "" {
					resp.OllamaLatestVersion = latest
					resp.OllamaUpdateAvailable = ai.CompareVersions(ver, latest) < 0
				}
			}
		}
	}

	return resp
}

// cachedLatestOllamaVersion returns the latest Ollama release tag, served
// from a long-TTL Redis cache. Unauthenticated GitHub allows only 60
// req/hr/IP, so we MUST NOT hit it on every admin panel render. On a cache
// miss we fetch once and store; on any fetch failure we return "" (the FE
// simply omits the update badge).
func cachedLatestOllamaVersion(ctx context.Context) string {
	if v, found, _ := redisStore.GetString(ctx, registry.AIOllamaLatest, nil); found {
		return v
	}
	// Bound the outbound GitHub call.
	gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	latest, _ := ai.LatestOllamaVersion(gctx)
	if latest != "" {
		_ = redisStore.SetString(ctx, registry.AIOllamaLatest, nil, latest)
	}
	return latest
}

// findOllamaProvider returns the first enabled Ollama provider, if any.
func findOllamaProvider(ctx context.Context) *aiModels.AIProvider {
	providers, err := aiModels.ListProviders(ctx)
	if err != nil {
		return nil
	}
	for _, p := range providers {
		if p.Kind == aiModels.KindOllama && p.Enabled {
			return p
		}
	}
	return nil
}

// reloadIfActive reloads the live service when the given provider is the
// active chat or embedding provider.
func reloadIfActive(ctx context.Context, providerID uuid.UUID) error {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return err
	}
	active := (settings.ChatProviderID != nil && *settings.ChatProviderID == providerID) ||
		(settings.EmbeddingProviderID != nil && *settings.EmbeddingProviderID == providerID)
	if active {
		return reloadAI(ctx)
	}
	return nil
}

// SetAgentDelegation stores the agent-to-agent delegation policy (whether one
// agent may hand work to another, how deep a chain may go, and where it is
// permitted) and reloads the service so the guard sees it on the next message
// rather than after a restart.
//
// A deployment-level veto (AI_AGENT_DELEGATION=false) is reported back as an error
// rather than silently accepted: an admin who cannot enable this needs to be told
// that infrastructure forbids it, not left with a toggle that saved and did
// nothing.
func SetAgentDelegation(ctx context.Context, enabled bool, maxHops int, surfaces string) error {
	if enabled && ai.DelegationVetoedByEnv() {
		return fmt.Errorf("agent-to-agent delegation is disabled for this deployment (AI_AGENT_DELEGATION)")
	}
	if err := aiModels.SetAgentDelegation(ctx, enabled, maxHops, surfaces); err != nil {
		return err
	}
	return reloadAI(ctx)
}

// ModelRoutingView is the routing screen: the purposes that exist and where
// each one currently runs.
type ModelRoutingView struct {
	Purposes []ai.Purpose                    `json:"purposes"`
	Routes   map[string]aiModels.RouteTarget `json:"routes"`
}

// GetModelRouting returns the purposes and their routes.
func GetModelRouting(ctx context.Context) (*ModelRoutingView, error) {
	routes, err := aiModels.GetModelRouting(ctx)
	if err != nil {
		return nil, err
	}
	return &ModelRoutingView{Purposes: ai.Purposes, Routes: routes}, nil
}

// ErrRouteNotAllowed is a route to a model that is not usable from the allowlist.
var ErrRouteNotAllowed = errors.New("that model is not on the allowlist, or it or its provider is switched off")

// SetModelRouting replaces the routing table. A purpose with no model goes back
// to the workspace default. Only allowlisted, enabled models may be routed to:
// the allowlist is the admin's statement of which models may see workspace
// content, and routing must not become a way around it.
func SetModelRouting(ctx context.Context, routes map[string]aiModels.RouteTarget) error {
	clean := map[string]aiModels.RouteTarget{}
	for purpose, t := range routes {
		if !ai.IsPurpose(purpose) {
			return fmt.Errorf("unknown kind of work %q", purpose)
		}
		t.Model = strings.TrimSpace(t.Model)
		if t.Model == "" {
			continue
		}
		am, err := aiModels.GetAuthorizedModelByProviderModel(ctx, t.ProviderID, t.Model)
		if err != nil || am == nil || !am.Enabled || !am.ProviderEnabled {
			return ErrRouteNotAllowed
		}
		clean[purpose] = t
	}
	if err := aiModels.SetModelRouting(ctx, clean); err != nil {
		return err
	}
	return reloadAI(ctx)
}
