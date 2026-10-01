package ai

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/akashc777/OneCamp/helpers"
)

// AIService is the main entry point for all AI operations.
// It holds the active LLM and embedding providers and provides
// convenience methods for common operations.
//
// An AIService instance is IMMUTABLE once constructed. To change the
// active provider/model at runtime (admin panel), a brand-new instance
// is built and atomically published via storeService. In-flight requests
// keep using the instance they captured, so a model swap never tears a
// running stream.
type AIService struct {
	LLM        LLMProvider
	Embedder   EmbeddingProvider
	Config     *AIConfig
	Resiliency *ResiliencyManager
	// Vision is the OPTIONAL multimodal client for image analysis, built only
	// when the admin configured a vision model. Nil otherwise.
	Vision LLMProvider
	// CodeRun is the OPTIONAL dedicated client for the code-PR runner's edit/
	// verify loop, built only when the admin selected a separate code-run model.
	// Nil otherwise (the code-run proxy then uses LLM, the chat model).
	CodeRun LLMProvider
	// models lazily builds + caches clients for non-default authorized models
	// a member may pick. Nil until the service is enabled.
	models *modelResolver
}

// servicePtr / configPtr hold the live service and config. They are
// swapped atomically on reload. Always read them through GetService /
// GetConfig — never capture the package var directly.
var (
	servicePtr atomic.Pointer[AIService]
	configPtr  atomic.Pointer[AIConfig]
	reloadMu   sync.Mutex // serializes reloads so two admins can't race
)

// ChatModelLabel names the model the workspace default currently resolves to,
// as "kind/model".
//
// A run that did not pin its own model used to record nothing, because the
// resolver returns an empty label for the default path. Empty is unhelpful in a
// ledger: it says the default was used, and the default is a setting that
// changes, so a year later nobody can say what it was at the time.
//
// Nil-safe, because callers reach it through GetService, which is nil before the
// service is initialised and whenever AI is switched off.
func (s *AIService) ChatModelLabel() string {
	if s == nil || s.Config == nil {
		return ""
	}
	return string(s.Config.Chat.Kind) + "/" + s.Config.Chat.Model
}

// GetService returns the live AI service (may be nil before init).
func GetService() *AIService { return servicePtr.Load() }

// init announces the AI subsystem to the feature registry that /config/client reports.
//
// In package init specifically, so that LINKING this package is what makes AI visible to
// the frontend. The AI-free v1 edition is built without these packages, so nothing
// registers, the endpoint reports no `ai` feature, and the frontend hides every AI entry
// point — with no edition-specific code in the endpoint or the client.
//
// The probe is evaluated per request rather than captured, because an admin can
// reconfigure and reload AI at runtime and IsEnabled changes with it. IsEnabled is
// nil-safe, which matters here: before the first successful init GetService returns nil,
// and "not ready yet" must read as unavailable rather than panic.
func init() {
	helpers.RegisterFeature(helpers.FeatureNameAI, func() bool {
		return GetService().IsEnabled()
	})
}

// GetConfig returns the live AI config (may be nil before init).
func GetConfig() *AIConfig { return configPtr.Load() }

func storeService(s *AIService) { servicePtr.Store(s) }
func storeConfig(c *AIConfig)   { configPtr.Store(c) }

// NewAIService creates a provider-agnostic AI service from a resolved
// config. The config carries normalized Chat and Embed endpoints, which
// may target different providers (e.g. chat on Anthropic, embeddings on
// local Ollama).
func NewAIService(config *AIConfig) (*AIService, error) {
	// Publish the local-only switch first so every provider client built below
	// (and every per-call streaming client) sees the live value at dial time.
	SetLocalOnlyMode(config.LocalOnlyMode)

	if !config.Enabled {
		helpers.MessageLogs.InfoLog.Println("AI service is disabled")
		return &AIService{Config: config}, nil
	}

	llm, err := buildLLM(config.Chat, config.EffectiveContextWindow(), 0, config.ReasoningEnabled, config.redactorForEndpoint(config.Chat))
	if err != nil {
		return nil, err
	}

	embedder, err := buildEmbedder(config.Embed, config)
	if err != nil {
		return nil, err
	}

	service := &AIService{
		LLM:        llm,
		Embedder:   embedder,
		Config:     config,
		Resiliency: NewResiliencyManager(config.RateLimitPerMin),
		models:     newModelResolver(),
	}

	// Build the optional vision client. A failure here must NOT break the
	// service - it just leaves image analysis unavailable.
	if config.HasVision() {
		if vc, verr := buildLLM(config.Vision, config.EffectiveContextWindow(), 0, false, config.redactorForEndpoint(config.Vision)); verr != nil {
			helpers.MessageLogs.ErrorLog.Printf("AI: failed to build vision client (%v); image analysis disabled", verr)
		} else {
			service.Vision = vc
			helpers.MessageLogs.InfoLog.Printf("AI vision model: %s/%s", config.Vision.Kind, config.Vision.Model)
		}
	}

	// Build the optional dedicated code-run client (reasoning off — coding
	// prompts don't use a thinking trace). A failure here must NOT break the
	// service; the code-run proxy just falls back to the chat model.
	if config.HasCodeRun() {
		if cc, cerr := buildLLM(config.CodeRun, config.EffectiveContextWindow(), 0, false, config.redactorForEndpoint(config.CodeRun)); cerr != nil {
			helpers.MessageLogs.ErrorLog.Printf("AI: failed to build code-run client (%v); code runs will use the chat model", cerr)
		} else {
			service.CodeRun = cc
			helpers.MessageLogs.InfoLog.Printf("AI code-run model: %s/%s", config.CodeRun.Kind, config.CodeRun.Model)
		}
	}

	helpers.MessageLogs.InfoLog.Printf("AI service initialized: chat=%s/%s embed=%s/%s(%dd)",
		config.Chat.Kind, config.Chat.Model, config.Embed.Kind, config.Embed.Model, config.EmbeddingDimension())

	return service, nil
}

// buildLLM constructs the chat/completion provider for an endpoint. numCtx
// (the resolved context window) is applied to the Ollama provider so the model
// is actually RUN with the admin-configured window — keeping it in lockstep
// with the prompt budget. reasoning sets the admin "thinking" mode for
// reasoning-capable Ollama models. 0/false leave the provider defaults.
// redactor (nil for local endpoints / redaction off) scrubs outbound PII for
// cloud endpoints.
func buildLLM(ep Endpoint, numCtx, maxOutput int, reasoning bool, redactor *Redactor) (LLMProvider, error) {
	opts := ProviderOptions{
		GuardSSRF:          ep.Kind == ProviderOpenAICompatible,
		InsecureSkipVerify: ep.InsecureTLS,
	}
	switch ep.Kind {
	case ProviderOllama:
		p := NewOllamaProviderWithDims(ep.BaseURL, ep.Model, "", 0)
		if numCtx > 0 {
			p.numCtx = numCtx
		}
		// maxOutput is deliberately NOT applied here. Ollama does not reject an
		// over-large num_predict — it simply generates fewer tokens — so clamping would
		// change nothing except hide the operator's stated value from the request.
		p.reasoning = reasoning
		p.redactor = redactor
		return p, nil
	case ProviderOpenAI:
		p := NewOpenAICompatibleProviderWithOpts(ep.APIKey, ep.Model, "", ep.BaseURL, 0, ProviderOpenAI, opts)
		p.reasoning = reasoning
		p.maxOutput = maxOutput
		p.redactor = redactor
		return p, nil
	case ProviderOpenAICompatible:
		if strings.TrimSpace(ep.BaseURL) == "" {
			return nil, fmt.Errorf("openai_compatible chat endpoint requires a base_url")
		}
		p := NewOpenAICompatibleProviderWithOpts(ep.APIKey, ep.Model, "", ep.BaseURL, 0, ProviderOpenAICompatible, opts)
		p.reasoning = reasoning
		p.maxOutput = maxOutput
		p.redactor = redactor
		return p, nil
	case ProviderAnthropic:
		p := NewAnthropicProviderWithOpts(ep.APIKey, ep.Model, ep.BaseURL, opts)
		p.maxOutput = maxOutput
		p.redactor = redactor
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported chat provider: %s", ep.Kind)
	}
}

// ProbeEmbeddingDimension builds a TRANSIENT embedder for the given endpoint
// and embeds a tiny probe string to measure the model's ACTUAL output vector
// dimension. It never touches the live service. Used by the admin embedding-
// model flow to verify the dimension the caller claims matches reality before
// committing it (and triggering a destructive reindex) — see Gap 3.
//
// The endpoint should carry the provider kind, base URL, decrypted API key,
// model, and TLS/SSRF posture. Dim is ignored (we are measuring it). A bounded
// context is the caller's responsibility.
func ProbeEmbeddingDimension(ctx context.Context, ep Endpoint, cfg *AIConfig) (int, error) {
	embedder, err := buildEmbedder(ep, cfg)
	if err != nil {
		return 0, err
	}
	vectors, err := embedder.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return 0, err
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return 0, fmt.Errorf("embedding model returned an empty vector")
	}
	return len(vectors[0]), nil
}

// buildEmbedder constructs the embedding provider for an endpoint.
// Anthropic has no embedding API, so an Anthropic embed endpoint falls
// back to the configured Ollama embedding settings.
func buildEmbedder(ep Endpoint, cfg *AIConfig) (EmbeddingProvider, error) {
	opts := ProviderOptions{
		GuardSSRF:          ep.Kind == ProviderOpenAICompatible,
		InsecureSkipVerify: ep.InsecureTLS,
	}
	redactor := cfg.redactorForEndpoint(ep)
	switch ep.Kind {
	case ProviderOllama:
		dims := ep.Dim
		if dims <= 0 {
			dims = 768
		}
		p := NewOllamaProviderWithDims(ep.BaseURL, "", ep.Model, dims)
		p.redactor = redactor
		return p, nil
	case ProviderOpenAI, ProviderOpenAICompatible:
		p := NewOpenAICompatibleProviderWithOpts(ep.APIKey, "", ep.Model, ep.BaseURL, ep.Dim, ep.Kind, opts)
		p.redactor = redactor
		return p, nil
	case ProviderAnthropic, "":
		// Anthropic (or unset) → Ollama fallback for embeddings so RAG works.
		helpers.MessageLogs.InfoLog.Println("Embedding provider falls back to Ollama (no native embeddings)")
		return NewOllamaProviderWithDims(cfg.OllamaHost, "", cfg.OllamaEmbeddingModel, cfg.EmbeddingDimension()), nil
	default:
		return nil, fmt.Errorf("unsupported embedding provider: %s", ep.Kind)
	}
}

// InitAIService loads configuration and initializes the global AI
// service. Call this once at application startup.
//
// It prefers DB-backed config (migration 64). If the DB settings row is
// unavailable (fresh DB before migrate, or a transient error) it falls
// back to environment variables so the service still boots — exactly the
// pre-existing behaviour.
func InitAIService() error {
	config := ResolveConfig(context.Background())

	service, err := NewAIService(config)
	if err != nil {
		return fmt.Errorf("failed to initialize AI service: %w", err)
	}

	storeConfig(config)
	storeService(service)
	return nil
}

// ReloadAIService rebuilds the service from the current DB settings and
// atomically swaps it in. Called after an admin changes the active
// provider/model. Serialized via reloadMu. On error the previous service
// is left untouched.
func ReloadAIService(ctx context.Context) error {
	reloadMu.Lock()
	defer reloadMu.Unlock()

	config := ResolveConfig(ctx)
	service, err := NewAIService(config)
	if err != nil {
		return fmt.Errorf("reload AI service: %w", err)
	}

	storeConfig(config)
	storeService(service)
	helpers.LogInfoWithContext(ctx, "AI service reloaded: provider=%s model=%s", config.Provider(), config.ActiveModel())
	return nil
}

// IsEnabled returns true if the AI service is configured and ready.
func (s *AIService) IsEnabled() bool {
	return s != nil && s.Config != nil && s.Config.Enabled && s.LLM != nil
}

// VisionClient returns the configured vision (multimodal) client and true
// when image analysis is available. Returns (nil, false) when no vision model
// is configured or the built client doesn't support the vision capability.
func (s *AIService) VisionClient() (VisionProvider, bool) {
	if s == nil || !s.IsEnabled() || s.Vision == nil {
		return nil, false
	}
	vp, ok := s.Vision.(VisionProvider)
	return vp, ok
}

// Summarize generates a summary of the given content on the model routed for
// summaries (the workspace default unless an admin routed them elsewhere).
// Meeting recaps and memory extraction name their own purpose via SummarizeFor.
func (s *AIService) Summarize(ctx context.Context, content string, systemPrompt string) (string, error) {
	return s.SummarizeFor(ctx, PurposeSummaries, content, systemPrompt)
}

// SummarizeWith is Summarize against a CALLER-CHOSEN client.
//
// Exists because a summary can be scoped to something that pins its own model — a
// channel with an admin-pinned model is the case that prompted it, where the summary was
// silently using the workspace default while agent runs in the same channel used the pin.
// A nil llm means "the workspace default", so callers with nothing to choose keep calling
// Summarize and nothing about their behaviour changes.
//
// The prompt shape and sampling options live here rather than at the call sites, so
// choosing a different client cannot accidentally come with a different temperature and
// make two summaries incomparable.
func (s *AIService) SummarizeWith(ctx context.Context, llm LLMProvider, content string, systemPrompt string) (string, error) {
	if !s.IsEnabled() {
		return "", fmt.Errorf("AI service is not enabled")
	}
	if llm == nil {
		llm = s.LLM
	}

	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: content},
	}

	opts := ChatOptions{
		Temperature: 0.3, // Lower temperature for factual summarization
		MaxTokens:   1024,
	}

	return llm.Chat(ctx, messages, opts)
}

// StreamChat is a convenience method for streaming chat responses.
func (s *AIService) StreamChat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	if !s.IsEnabled() {
		return nil, fmt.Errorf("AI service is not enabled")
	}

	return s.LLM.ChatStream(ctx, messages, opts)
}

// embeddingSpendPolicy says whether an embedding call may be REFUSED when a
// daily token cap is already exhausted. Both policies are metered; they differ
// only in refusability, because the cost of refusing is not the same on the two
// paths.
type embeddingSpendPolicy bool

const (
	// refusableEmbedding is for on-demand embeddings (a search query). Refusing
	// costs exactly that one call: the caller sees a budget error and retries
	// after the daily reset or an admin raises the cap.
	refusableEmbedding embeddingSpendPolicy = true

	// unrefusableEmbedding is for the indexing path. Refusing here would leave
	// the content with no vector long after the budget resets, so it would stay
	// permanently unfindable by meaning — a silent, unbounded correctness loss
	// that is worse than the spend it saves. Metered, never blocked.
	unrefusableEmbedding embeddingSpendPolicy = false
)

// GenerateEmbeddings embeds texts for on-demand work such as a search query.
// Budget-guarded and metered, like every other provider call.
func (s *AIService) GenerateEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts, refusableEmbedding)
}

// GenerateEmbeddingsForIndexing embeds texts on the indexing path. Metered like
// every other provider call but deliberately never refused on budget — see
// unrefusableEmbedding for why.
func (s *AIService) GenerateEmbeddingsForIndexing(ctx context.Context, texts []string) ([][]float32, error) {
	return s.embed(ctx, texts, unrefusableEmbedding)
}

// embed is the single embedding chokepoint: availability check, budget guard,
// provider call, spend metering. Every embedding in the product goes through
// here (enforced by TestEmbeddingsGoThroughTheMeteredChokepoint) so a new
// caller cannot reintroduce uncapped, uncounted spend.
//
// The Embedder interface returns no usage counts, so spend is the calibrated
// estimate of what was sent — the same fallback the streaming chat path uses.
func (s *AIService) embed(ctx context.Context, texts []string, policy embeddingSpendPolicy) ([][]float32, error) {
	if !s.IsEnabled() || s.Embedder == nil {
		return nil, fmt.Errorf("AI embedding service is not available")
	}

	if policy == refusableEmbedding {
		if err := guardTokenBudget(ctx); err != nil {
			return nil, err
		}
	}

	vectors, err := s.Embedder.Embed(ctx, texts)
	if err != nil {
		// Meter successes only, matching the chat chokepoint: a failed call has
		// no usable result, and counting transport failures would let a broken
		// provider burn the whole daily cap on retries.
		return nil, err
	}

	spend := 0
	for _, t := range texts {
		spend += EstimateTokens(t)
	}
	RecordTokenSpend(ctx, spend)

	return vectors, nil
}
