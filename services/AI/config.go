package ai

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// Endpoint is a fully-resolved description of a single model connection.
// Chat and embedding each have their own Endpoint, so the admin can mix
// providers (e.g. chat on Anthropic, embeddings on local Ollama).
type Endpoint struct {
	Kind        ProviderType `json:"kind"`     // ollama | openai | anthropic | openai_compatible
	BaseURL     string       `json:"base_url"` // empty → provider default
	APIKey      string       `json:"-"`        // never serialized
	Model       string       `json:"model"`
	Dim         int          `json:"dim,omitempty"`          // embedding dimension (embed endpoint only)
	InsecureTLS bool         `json:"insecure_tls,omitempty"` // opt-in self-signed TLS
}

// AIConfig holds the resolved AI configuration that NewAIService builds
// from. It carries the normalized Chat/Embed endpoints plus a handful of
// legacy env-derived fields kept for backwards compatibility.
type AIConfig struct {
	Enabled         bool `json:"enabled"`
	RateLimitPerMin int  `json:"rate_limit_per_min"`

	// ContextWindowTokens is the admin-set model context window (tokens).
	// 0 means "use the env/default" (OLLAMA_NUM_CTX or 8192). Source of
	// truth for BOTH the prompt token budget and the Ollama provider's
	// num_ctx, so the two can never diverge.
	ContextWindowTokens int `json:"context_window_tokens"`

	// WorkspaceDailyTokenBudget / UserDailyTokenBudget cap daily AI token
	// spend workspace-wide and per user (0 = unlimited). Admin-managed and
	// hot-reloaded; the env vars AI_WORKSPACE_DAILY_TOKEN_BUDGET /
	// AI_USER_DAILY_TOKEN_BUDGET are a boot fallback used until an admin sets
	// a value. Enforced at the provider layer (see budget.go).
	WorkspaceDailyTokenBudget int `json:"workspace_daily_token_budget"`
	UserDailyTokenBudget      int `json:"user_daily_token_budget"`

	// ReasoningEnabled turns on "thinking" mode for reasoning-capable models
	// (gemma4/deepseek-r1/qwen3). Admin-managed; false (default) keeps the
	// fast no-thinking behaviour. Flows into the provider's per-request think
	// flag. Ignored by non-thinking models.
	ReasoningEnabled bool `json:"reasoning_enabled"`

	// LocalOnlyMode is the premium data-residency guarantee: when true, the
	// provider dial guard refuses any non-local model endpoint, so no workspace
	// content can leave to a cloud model. Admin-managed; default false.
	LocalOnlyMode bool `json:"local_only_mode"`

	// PIIRedactionEnabled scrubs detected PII from outbound prompts before they
	// reach a NON-LOCAL (cloud) model. Local endpoints are never redacted.
	// Admin-managed; default false. Independent of LocalOnlyMode (that blocks
	// cloud entirely; this is for installs that still allow cloud but want PII
	// stripped first).
	PIIRedactionEnabled bool `json:"pii_redaction_enabled"`
	// PIICustomPatterns are admin-defined regexes applied in addition to the
	// built-in detectors. Invalid patterns are skipped at compile time.
	PIICustomPatterns []string `json:"pii_custom_patterns,omitempty"`

	// Normalized endpoints — the source of truth for NewAIService.
	Chat  Endpoint `json:"chat"`
	Embed Endpoint `json:"embed"`
	// Vision is the OPTIONAL multimodal endpoint for image analysis. Zero
	// value (empty Model/Kind) means vision is disabled.
	Vision Endpoint `json:"vision"`
	// CodeRun is the OPTIONAL dedicated endpoint the code-PR coding runner uses
	// for its edit/verify loop, so code runs don't compete with the chat model's
	// provider quota. Zero value means "use the chat model" (Chat).
	CodeRun Endpoint `json:"code_run"`

	// Legacy env-derived Ollama tuning (still read by the provider via
	// env, retained here for status reporting / fallback construction).
	OllamaHost           string `json:"ollama_host"`
	OllamaModel          string `json:"ollama_model"`
	OllamaEmbeddingModel string `json:"ollama_embedding_model"`
	OllamaNumThread      int    `json:"ollama_num_thread"`
	OllamaNumCtx         int    `json:"ollama_num_ctx"`
	OllamaKeepAlive      string `json:"ollama_keep_alive"`

	OpenAIAPIKey         string `json:"-"`
	OpenAIModel          string `json:"openai_model"`
	OpenAIEmbeddingModel string `json:"openai_embedding_model"`

	AnthropicAPIKey string `json:"-"`
	AnthropicModel  string `json:"anthropic_model"`

	// Web search (provider-agnostic). Resolved from ai_settings (admin) with
	// env (AI_WEB_SEARCH_*) as a boot fallback. WebSearchAPIKey is never
	// serialized. WebSearchEnabled is the admin on/off switch.
	WebSearchProvider string `json:"web_search_provider"`
	WebSearchBaseURL  string `json:"web_search_base_url"`
	WebSearchAPIKey   string `json:"-"`
	WebSearchEnabled  bool   `json:"web_search_enabled"`

	// SandboxEnabled is the agent execution-sandbox master switch (admin). When
	// false the run_analysis tool refuses; hot-reloaded with the rest of config.
	SandboxEnabled bool `json:"sandbox_enabled"`

	// CodePREnabled is the agent code-PR master switch (admin). When false the
	// code_pr tool refuses and is hidden from the builder; hot-reloaded with the
	// rest of config. Runner endpoint/budgets are read per-run from settings by
	// the business layer.
	CodePREnabled bool `json:"code_pr_enabled"`

	// Agent-to-agent delegation policy (admin). Carried in the hot-reloaded
	// config rather than read per message: the guard runs on the message path,
	// where a DB round trip per candidate agent would be the wrong trade.
	AgentDelegationEnabled  bool   `json:"agent_delegation_enabled"`
	AgentDelegationMaxHops  int    `json:"agent_delegation_max_hops"`
	AgentDelegationSurfaces string `json:"agent_delegation_surfaces"`

	// Routing sends each kind of background work to an allowlisted model
	// (migration 172). Absent purposes use the default. See routing.go.
	Routing map[string]aiModels.RouteTarget `json:"routing,omitempty"`
}

// SandboxEnabled reports whether the agent execution sandbox is turned on.
func SandboxEnabled() bool {
	c := GetConfig()
	return c != nil && c.SandboxEnabled
}

// CodePREnabled reports whether the agent code-PR feature is turned on.
func CodePREnabled() bool {
	c := GetConfig()
	return c != nil && c.CodePREnabled
}

// DelegationVetoedByEnv reports a DEPLOYMENT-LEVEL refusal of agent-to-agent
// delegation (AI_AGENT_DELEGATION=false|0|no|off).
//
// Deliberately asymmetric: the env var can only turn delegation OFF, never on. A
// self-hoster who has decided agents must never talk to each other can enforce it
// from infrastructure without depending on nobody flipping an admin toggle, while
// an absent var means "no opinion, let the admin decide" rather than "enabled".
//
// Lives here rather than in business/AIAgent so both the guard and the admin
// config surface can consult it — business/AI cannot import business/AIAgent
// (AIAgent already imports business/AI), and duplicating the parse in two layers
// is how the UI and the enforcement drift apart.
func DelegationVetoedByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AI_AGENT_DELEGATION"))) {
	case "false", "0", "no", "off":
		return true
	default:
		return false
	}
}

// AgentDelegationPolicy reports the admin-configured agent-to-agent delegation
// policy: whether it is on, how deep a chain may go, and where it is permitted.
//
// Returns the safe answer (off) when config is unavailable, so a delegation can
// never be authorised by a missing config — the same posture as every other
// switch here.
func AgentDelegationPolicy() (enabled bool, maxHops int, surfaces string) {
	c := GetConfig()
	if c == nil {
		return false, 0, ""
	}
	return c.AgentDelegationEnabled, c.AgentDelegationMaxHops, c.AgentDelegationSurfaces
}

// LoadAIConfig reads AI configuration from environment variables with
// sensible defaults. This is the legacy env-only path, used as a
// fallback when DB-backed config (migration 64) is unavailable.
func LoadAIConfig() *AIConfig {
	provider := ProviderType(getEnvStr("AI_PROVIDER", "ollama"))

	ollamaHost := getEnvStr("OLLAMA_HOST", "http://localhost:11434")
	// qwen3:4b-instruct, not llama3.2:3b: on a 4-core CPU it passed 12 of 15
	// agent jobs against 6 (tasks from requests, answering without a tool,
	// summaries, first steps, JSON) at 5.5 s a reply (benchmark, 3 Oct 2026).
	ollamaModel := getEnvStr("OLLAMA_MODEL", "qwen3:4b-instruct")
	ollamaEmbed := getEnvStr("OLLAMA_EMBEDDING_MODEL", "nomic-embed-text")
	openaiKey := getEnvStr("OPENAI_API_KEY", "")
	openaiModel := getEnvStr("OPENAI_MODEL", "gpt-4o-mini")
	openaiEmbed := getEnvStr("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small")
	anthropicKey := getEnvStr("ANTHROPIC_API_KEY", "")
	anthropicModel := getEnvStr("ANTHROPIC_MODEL", "claude-3-5-sonnet-latest")

	// A HOSTED PROVIDER WITH NO KEY STAYS THAT PROVIDER. It used to be silently
	// rewritten to ollama here, which is the wrong answer twice over.
	//
	// On a deployment that runs no local engine — every bring-your-own-key
	// install, where Ollama is not even in the compose file — the rewrite pointed
	// chat at a host that answers nothing, so a missing API key surfaced as a
	// connection error to an address the operator never configured. Nothing in
	// that message says "add your key".
	//
	// On a deployment that DOES run a local engine it is worse than confusing:
	// the operator asked for OpenAI or Anthropic, and prompts would quietly go to
	// a different model instead. Choosing a provider is often a data-handling
	// decision, and silently substituting another one is the single worst
	// outcome this function has.
	//
	// So the selection is honoured and the gap is logged here; chat then refuses
	// with a message that names the missing key.
	switch provider {
	case ProviderOpenAI:
		if openaiKey == "" {
			helpers.MessageLogs.ErrorLog.Println(
				"AI_PROVIDER is openai but OPENAI_API_KEY is not set. Chat will refuse until a key " +
					"is added here or in the admin AI panel; no other provider is substituted.")
		}
	case ProviderAnthropic:
		if anthropicKey == "" {
			helpers.MessageLogs.ErrorLog.Println(
				"AI_PROVIDER is anthropic but ANTHROPIC_API_KEY is not set. Chat will refuse until a " +
					"key is added here or in the admin AI panel; no other provider is substituted.")
		}
	}

	config := &AIConfig{
		Enabled:         getEnvBool("AI_ENABLED", true),
		RateLimitPerMin: getEnvInt("AI_RATE_LIMIT_PER_MIN", 30),

		// Honor the env pin even in the degraded env-only boot path.
		LocalOnlyMode: envLocalOnlyPinned(),

		OllamaHost:           ollamaHost,
		OllamaModel:          ollamaModel,
		OllamaEmbeddingModel: ollamaEmbed,
		OllamaNumThread:      getEnvInt("OLLAMA_NUM_THREAD", 0),
		OllamaNumCtx:         getEnvInt("OLLAMA_NUM_CTX", 8192),
		OllamaKeepAlive:      getEnvStr("OLLAMA_KEEP_ALIVE", "5m"),

		OpenAIAPIKey:         openaiKey,
		OpenAIModel:          openaiModel,
		OpenAIEmbeddingModel: openaiEmbed,

		AnthropicAPIKey: anthropicKey,
		AnthropicModel:  anthropicModel,

		// Web search env fallback (used when DB config is unavailable). The
		// DB-backed admin settings override these in BuildConfigFromDB.
		WebSearchProvider: strings.ToLower(strings.TrimSpace(getEnvStr("AI_WEB_SEARCH_PROVIDER", ""))),
		WebSearchBaseURL:  strings.TrimRight(strings.TrimSpace(getEnvStr("AI_WEB_SEARCH_BASE_URL", "")), "/"),
		WebSearchAPIKey:   strings.TrimSpace(getEnvStr("AI_WEB_SEARCH_API_KEY", "")),
		// In the env path, a configured provider is treated as enabled.
		WebSearchEnabled: strings.TrimSpace(getEnvStr("AI_WEB_SEARCH_PROVIDER", "")) != "",
	}

	// Build chat endpoint from the selected provider.
	switch provider {
	case ProviderOpenAI:
		config.Chat = Endpoint{Kind: ProviderOpenAI, APIKey: openaiKey, Model: openaiModel}
	case ProviderAnthropic:
		config.Chat = Endpoint{Kind: ProviderAnthropic, APIKey: anthropicKey, Model: anthropicModel}
	default:
		config.Chat = Endpoint{Kind: ProviderOllama, BaseURL: ollamaHost, Model: ollamaModel}
	}

	// Embeddings: OpenAI provides them; Anthropic does not (fall back to
	// Ollama); Ollama provides them natively.
	switch provider {
	case ProviderOpenAI:
		config.Embed = Endpoint{Kind: ProviderOpenAI, APIKey: openaiKey, Model: openaiEmbed, Dim: openAIEmbedDim(openaiEmbed)}
	default:
		config.Embed = Endpoint{Kind: ProviderOllama, BaseURL: ollamaHost, Model: ollamaEmbed, Dim: 768}
	}

	helpers.MessageLogs.InfoLog.Printf("AI configuration loaded from env: chat=%s/%s embed=%s/%s enabled=%v",
		config.Chat.Kind, config.Chat.Model, config.Embed.Kind, config.Embed.Model, config.Enabled)

	return config
}

// ActiveModel returns the active chat model name.
func (c *AIConfig) ActiveModel() string { return c.Chat.Model }

// endpointHost returns the host an endpoint dials, applying provider defaults
// when the base URL is empty (env-only config path).
func (c *AIConfig) endpointHost(ep Endpoint) string {
	base := ep.BaseURL
	if base == "" {
		switch ep.Kind {
		case ProviderOllama:
			base = c.OllamaHost
		case ProviderOpenAI:
			return "api.openai.com"
		case ProviderAnthropic:
			return "api.anthropic.com"
		default:
			return ""
		}
	}
	if u, err := url.Parse(base); err == nil {
		return u.Hostname()
	}
	return ""
}

// NonLocalActiveEndpoints returns human-readable labels for any active model
// endpoint (chat / embeddings / vision) that is NOT on local infrastructure.
// Used to refuse enabling local-only mode while a cloud endpoint is selected,
// so the admin gets a clear message instead of silently-broken AI.
func (c *AIConfig) NonLocalActiveEndpoints() []string {
	var out []string
	check := func(label string, ep Endpoint) {
		if ep.Kind == "" && ep.Model == "" {
			return // unset (e.g. vision disabled)
		}
		host := c.endpointHost(ep)
		local, err := helpers.IsLocalNetworkHost(host)
		if err != nil || !local {
			out = append(out, fmt.Sprintf("%s (%s)", label, ep.Kind))
		}
	}
	check("chat", c.Chat)
	check("embeddings", c.Embed)
	if c.HasVision() {
		check("vision", c.Vision)
	}
	return out
}

// endpointIsCloud reports whether an endpoint can transmit content off the
// customer's infrastructure. It fails CLOSED (treats resolution failure as
// cloud), so an ambiguous host is redacted rather than leaked. Used to decide
// whether the PII redaction pass applies to a provider's outbound content.
func (c *AIConfig) endpointIsCloud(ep Endpoint) bool {
	host := c.endpointHost(ep)
	local, err := helpers.IsLocalNetworkHost(host)
	if err != nil {
		return true // fail closed: when in doubt, redact
	}
	return !local
}

// EndpointCloudCheck reports whether a provider selection (kind + base URL)
// would be a non-local (cloud) endpoint under the live config's host defaults.
// Exported for the business layer's config-time local-only guard. Fails closed
// (treats an unresolvable host as cloud).
func EndpointCloudCheck(kind ProviderType, baseURL string) bool {
	cfg := GetConfig()
	if cfg == nil {
		cfg = LoadAIConfig()
	}
	return cfg.endpointIsCloud(Endpoint{Kind: kind, BaseURL: baseURL})
}

// redactorForEndpoint returns a compiled Redactor when PII redaction is enabled
// AND the endpoint is cloud-bound; otherwise nil (local path / redaction off
// → no redaction, no DNS cost). Built once per provider client at construction.
func (c *AIConfig) redactorForEndpoint(ep Endpoint) *Redactor {
	if c == nil || !c.PIIRedactionEnabled {
		return nil
	}
	if !c.endpointIsCloud(ep) {
		return nil
	}
	return NewRedactor(c.PIICustomPatterns)
}

// ActiveEmbeddingModel returns the active embedding model name.
func (c *AIConfig) ActiveEmbeddingModel() string { return c.Embed.Model }

// Provider returns the active chat provider kind (for status reporting).
func (c *AIConfig) Provider() ProviderType { return c.Chat.Kind }

// HasVision reports whether an optional vision (multimodal) model is
// configured. When false, image analysis is unavailable.
func (c *AIConfig) HasVision() bool {
	return c != nil && c.Vision.Model != "" && c.Vision.Kind != ""
}

// HasCodeRun reports whether an optional dedicated code-run model is configured.
// When false, the code-PR runner uses the chat model.
func (c *AIConfig) HasCodeRun() bool {
	return c != nil && c.CodeRun.Model != "" && c.CodeRun.Kind != ""
}

// EffectiveContextWindow resolves the usable model context window in tokens:
// the admin-set value (ContextWindowTokens) when > 0, else the env-derived
// OllamaNumCtx (OLLAMA_NUM_CTX), else the 8192 default. Floored at
// minContextWindow so a misconfigured tiny value can't starve the prompt.
// This is the single source consulted by BOTH the token budget and the
// Ollama provider, so they never disagree.
func (c *AIConfig) EffectiveContextWindow() int {
	v := c.ContextWindowTokens
	if v <= 0 {
		v = c.OllamaNumCtx
	}
	if v <= 0 {
		v = defaultContextWindow
	}
	if v < minContextWindow {
		v = minContextWindow
	}
	return v
}

// EmbeddingDimension returns the configured embedding vector dimension.
// Defaults to 768 (nomic-embed-text) when unset. MUST match the
// OpenSearch ai_embeddings index dimension.
func (c *AIConfig) EmbeddingDimension() int {
	if c.Embed.Dim > 0 {
		return c.Embed.Dim
	}
	return 768
}

// openAIEmbedDim returns the known dimension for OpenAI embedding models.
func openAIEmbedDim(model string) int {
	if model == "" {
		return 1536
	}
	if strings.Contains(strings.ToLower(model), "large") {
		return 3072
	}
	return 1536
}

// --- Env helpers ---

func getEnvStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// StreamTimeout is the wall-clock budget for a streaming AI response (the SSE
// handlers). Local models on CPU can be slow to load and emit the first token,
// so this is admin-tunable via AI_STREAM_TIMEOUT_SECONDS. Defaults to 120s,
// floored at 30s and capped at 600s to keep a misconfiguration from creating
// zombie connections.
func StreamTimeout() time.Duration {
	secs := getEnvInt("AI_STREAM_TIMEOUT_SECONDS", 120)
	if secs < 30 {
		secs = 30
	}
	if secs > 600 {
		secs = 600
	}
	return time.Duration(secs) * time.Second
}
