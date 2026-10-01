package adapter

// Request/response DTOs for the admin-managed, model-agnostic AI
// configuration API (migration 64). These are the border types between
// the HTTP layer and the FE admin panel. API keys are NEVER returned to
// the FE — only a has_api_key boolean.

// ProviderView is a provider row as shown to the admin (no secret).
type ProviderView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // ollama | openai | anthropic | openai_compatible
	Label       string `json:"label"`
	BaseURL     string `json:"base_url"`
	HasAPIKey   bool   `json:"has_api_key"`
	Enabled     bool   `json:"enabled"`
	IsBuiltin   bool   `json:"is_builtin"`
	InsecureTLS bool   `json:"insecure_tls"`
	// KeyUnreadable means a key IS stored but will not decrypt, so the provider cannot be used
	// until it is re-entered. See aiModels.ErrProviderKeyUnreadable.
	//
	// SEPARATE FROM HasAPIKey ON PURPOSE, and the pair only makes sense together. An unreadable
	// key is reported as HasAPIKey=false, because a key that cannot be decrypted is as useless as
	// none — but "none was ever set" and "the one you set is now unreadable" call for different
	// things from the admin, and only the second names AI_CONFIG_KEK as the likely cause.
	//
	// Without this field the admin UI could not tell them apart, and for a custom
	// openai_compatible provider it showed no key state at all: the editor's "key required" badge
	// is only for the hosted built-ins, so a broken provider looked completely normal while every
	// request through it failed. That was the observed beta symptom.
	KeyUnreadable bool   `json:"key_unreadable,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// ModelView is one model in a provider's catalog.
type ModelView struct {
	ID        string `json:"id"`
	Installed bool   `json:"installed"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Embedding bool   `json:"embedding,omitempty"`
}

// AIConfigResponse is the full admin config payload (GET /admin/ai/config).
type AIConfigResponse struct {
	Enabled         bool           `json:"enabled"`
	RateLimitPerMin int            `json:"rate_limit_per_min"`
	Providers       []ProviderView `json:"providers"`

	// Active selections.
	ChatProviderID      string `json:"chat_provider_id"`
	ChatModel           string `json:"chat_model"`
	EmbeddingProviderID string `json:"embedding_provider_id"`
	EmbeddingModel      string `json:"embedding_model"`
	EmbeddingDimension  int    `json:"embedding_dimension"`

	// Vision (optional multimodal model for image analysis). Empty = disabled.
	VisionProviderID string `json:"vision_provider_id"`
	VisionModel      string `json:"vision_model"`

	// ContextWindowTokens is the admin-set model context window (0 = use the
	// env/default). EffectiveContextWindow is what's actually in force after
	// applying the env/default fallback + floor, shown to the admin as the
	// resolved value.
	ContextWindowTokens    int `json:"context_window_tokens"`
	EffectiveContextWindow int `json:"effective_context_window"`

	// WorkspaceDailyTokenBudget / UserDailyTokenBudget are the admin-set daily
	// AI token caps (0 = unlimited), enforced across all AI features.
	WorkspaceDailyTokenBudget int `json:"workspace_daily_token_budget"`
	UserDailyTokenBudget      int `json:"user_daily_token_budget"`

	// ReasoningEnabled is the admin "thinking" mode for reasoning-capable
	// models (gemma4/deepseek-r1/qwen3). false = faster (no chain-of-thought).
	ReasoningEnabled bool `json:"reasoning_enabled"`

	// LocalOnlyMode is the data-residency guarantee (no content to cloud
	// models). LocalOnlyPinnedByEnv reports whether it is forced on by env
	// (so the UI disables the toggle).
	LocalOnlyMode        bool `json:"local_only_mode"`
	LocalOnlyPinnedByEnv bool `json:"local_only_pinned_by_env"`

	// Agent-to-agent delegation policy. AgentDelegationVetoedByEnv reports a
	// DEPLOYMENT-LEVEL refusal (AI_AGENT_DELEGATION=false): the UI must disable the
	// toggle and say so, because otherwise an admin flips it, sees it save, and
	// nothing happens — the worst kind of setting.
	AgentDelegationEnabled     bool   `json:"agent_delegation_enabled"`
	AgentDelegationMaxHops     int    `json:"agent_delegation_max_hops"`
	AgentDelegationSurfaces    string `json:"agent_delegation_surfaces"`
	AgentDelegationVetoedByEnv bool   `json:"agent_delegation_vetoed_by_env"`

	// PIIRedactionEnabled scrubs PII from outbound prompts before they reach a
	// cloud model. PIICustomPatterns are admin-defined regexes (one per line).
	PIIRedactionEnabled bool   `json:"pii_redaction_enabled"`
	PIICustomPatterns   string `json:"pii_custom_patterns"`

	// Ambient agents.
	MeetingRecapEnabled      bool   `json:"meeting_recap_enabled"`
	MeetingNotesDocEnabled   bool   `json:"meeting_notes_doc_enabled"`
	MeetingRecapInstructions string `json:"meeting_recap_instructions"`
	MemoryLayerEnabled       bool   `json:"memory_layer_enabled"`
	TeamReportEnabled        bool   `json:"team_report_enabled"`
	NudgesEnabled            bool   `json:"nudges_enabled"`
	CoworkerEnabled          bool   `json:"coworker_enabled"`

	// CodeAnalysisMaxFiles is the admin-set per-analysis file budget for the
	// code-aware bug agent (0 = use the default). EffectiveCodeAnalysisMaxFiles
	// is the resolved value actually in force after clamping.
	CodeAnalysisMaxFiles          int `json:"code_analysis_max_files"`
	EffectiveCodeAnalysisMaxFiles int `json:"effective_code_analysis_max_files"`

	// IssueTriageEnabled auto-analyzes newly-opened GitHub issues and posts the
	// proposed fix as an internal task comment. Opt-in (default false).
	IssueTriageEnabled bool `json:"issue_triage_enabled"`

	// Web search (provider-agnostic). Provider id, base URL, enabled switch,
	// and whether an API key is stored (the key itself is never returned).
	WebSearchProvider string `json:"web_search_provider"`
	WebSearchBaseURL  string `json:"web_search_base_url"`
	WebSearchEnabled  bool   `json:"web_search_enabled"`
	HasWebSearchKey   bool   `json:"has_web_search_key"`

	// Agent execution sandbox. RunnerURL/HasRunnerToken describe the isolated
	// code-runner sidecar; the token itself is never returned. The daily
	// budgets are workspace/channel caps (0 = unlimited); per-agent caps live
	// on the agent row. ImageDigest pins the runner image for auditability.
	SandboxEnabled               bool   `json:"sandbox_enabled"`
	SandboxRunnerURL             string `json:"sandbox_runner_url"`
	HasSandboxRunnerToken        bool   `json:"has_sandbox_runner_token"`
	SandboxImageDigest           string `json:"sandbox_image_digest"`
	SandboxWorkspaceDailySeconds int    `json:"sandbox_workspace_daily_seconds"`
	SandboxWorkspaceDailyRuns    int    `json:"sandbox_workspace_daily_runs"`
	SandboxChannelDailySeconds   int    `json:"sandbox_channel_daily_seconds"`
	SandboxChannelDailyRuns      int    `json:"sandbox_channel_daily_runs"`
	// Today's workspace-wide sandbox consumption (runner seconds + run count),
	// so the admin can see live spend against the budgets above.
	SandboxUsedTodaySeconds int `json:"sandbox_used_today_seconds"`
	SandboxUsedTodayRuns    int `json:"sandbox_used_today_runs"`

	// Agent code-PR. RunnerURL/HasRunnerToken describe the coding-capable
	// code-runner sidecar; the token itself is never returned. EgressAllowlist
	// is the set of hosts the runner may reach (default-deny otherwise).
	// OutOfScopePolicy is "flag_open" | "pause". DraftOnRed opens a draft PR
	// when a change can't reach green. Daily budgets are workspace/channel caps
	// in MINUTES and runs (0 = unlimited); per-agent caps live on the agent row.
	CodePREnabled               bool     `json:"code_pr_enabled"`
	CodePRRunnerURL             string   `json:"code_pr_runner_url"`
	HasCodePRRunnerToken        bool     `json:"has_code_pr_runner_token"`
	CodePREgressAllowlist       []string `json:"code_pr_egress_allowlist"`
	CodePROutOfScopePolicy      string   `json:"code_pr_out_of_scope_policy"`
	CodePRDraftOnRed            bool     `json:"code_pr_draft_on_red"`
	CodePRWorkspaceDailyMinutes int      `json:"code_pr_workspace_daily_minutes"`
	CodePRWorkspaceDailyRuns    int      `json:"code_pr_workspace_daily_runs"`
	CodePRChannelDailyMinutes   int      `json:"code_pr_channel_daily_minutes"`
	CodePRChannelDailyRuns      int      `json:"code_pr_channel_daily_runs"`
	// CodePRAllowUnlinked lets the agent work on any repo the connected GitHub
	// account can reach (verified per-run), not only project-linked repos.
	// Default false (linked-only) — the conservative, blast-radius-safe default.
	CodePRAllowUnlinked bool `json:"code_pr_allow_unlinked"`
	// CodePRWallMinutes is how long ONE coding run may work before it wraps up
	// and hands back whatever it finished (partial work still pushed to a branch).
	// 0 = use the built-in default. CodePREffectiveWallMinutes is the limit
	// actually in force after the default/env fallback and clamping, so the admin
	// always sees the real value.
	CodePRWallMinutes          int `json:"code_pr_wall_minutes"`
	CodePREffectiveWallMinutes int `json:"code_pr_effective_wall_minutes"`
	// Optional dedicated model for the coding runner (empty = use the chat
	// model), so code runs don't compete with the chat model's provider quota.
	CodePRChatProviderID string `json:"code_pr_chat_provider_id"`
	CodePRChatModel      string `json:"code_pr_chat_model"`
	// Today's workspace-wide code-PR consumption (runner minutes + run count).
	CodePRUsedTodayMinutes int `json:"code_pr_used_today_minutes"`
	CodePRUsedTodayRuns    int `json:"code_pr_used_today_runs"`

	// Live runtime health (mirrors GET /ai/status for convenience).
	CircuitState string `json:"circuit_state"`
}

// CreateProviderRequest adds a custom OpenAI-compatible endpoint.
type CreateProviderRequest struct {
	Label       string `json:"label"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key,omitempty"`
	InsecureTLS bool   `json:"insecure_tls,omitempty"`
}

// UpdateProviderRequest edits an existing provider. Omitted fields are
// left unchanged. api_key semantics: omit = keep, "" = clear, value = set.
type UpdateProviderRequest struct {
	Label       *string `json:"label,omitempty"`
	BaseURL     *string `json:"base_url,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	APIKey      *string `json:"api_key,omitempty"`
	InsecureTLS *bool   `json:"insecure_tls,omitempty"`
}

// SetChatModelRequest sets the active chat provider+model.
type SetChatModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// SetVisionModelRequest sets (or clears) the optional vision provider+model
// for image analysis. An empty provider_id or model clears it (vision off).
type SetVisionModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// SetCodePRModelRequest sets (or clears) the optional dedicated model the
// code-PR coding runner uses. An empty provider_id or model clears it (code
// runs then fall back to the chat model).
type SetCodePRModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// SetEmbeddingModelRequest sets the active embedding provider+model.
// Dimension is required so the server can validate/reindex against the
// OpenSearch k-NN index.
type SetEmbeddingModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Dimension  int    `json:"dimension"`
	// Reindex, when true, authorizes the heavy re-embed of all content if
	// the new dimension differs from the live index. Without it, a
	// dimension change is rejected to prevent silently breaking search.
	Reindex bool `json:"reindex"`
}

// SetEnabledRequest toggles the global AI switch.
type SetEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// SetMeetingRecapRequest toggles the post-call recap ambient agent.
type SetMeetingRecapRequest struct {
	Enabled bool `json:"enabled"`
}

// SetMeetingRecapInstructionsRequest sets the optional custom recap guidance.
type SetMeetingRecapInstructionsRequest struct {
	Instructions string `json:"instructions"`
}

// SetWebSearchRequest configures the provider-agnostic web search. APIKey is
// applied only when non-empty (so saving without re-entering keeps the stored
// one); ClearKey removes the stored key. Provider "" disables the tool.
type SetWebSearchRequest struct {
	Provider string `json:"provider"` // "" | searxng | tavily | brave
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key,omitempty"`
	Enabled  bool   `json:"enabled"`
	ClearKey bool   `json:"clear_key,omitempty"`
}

// SetSandboxConfigRequest configures the agent execution sandbox. RunnerToken is
// applied only when non-empty (so saving without re-entering keeps the stored
// one); ClearToken removes the stored token. The daily budgets are
// workspace/channel caps in seconds and runs (0 = unlimited); the business
// layer clamps them to safe bounds. An empty RunnerURL while Enabled is true is
// rejected (the tool would have nowhere to run).
type SetSandboxConfigRequest struct {
	Enabled               bool   `json:"enabled"`
	RunnerURL             string `json:"runner_url"`
	RunnerToken           string `json:"runner_token,omitempty"`
	ClearToken            bool   `json:"clear_token,omitempty"`
	ImageDigest           string `json:"image_digest"`
	WorkspaceDailySeconds int    `json:"workspace_daily_seconds"`
	WorkspaceDailyRuns    int    `json:"workspace_daily_runs"`
	ChannelDailySeconds   int    `json:"channel_daily_seconds"`
	ChannelDailyRuns      int    `json:"channel_daily_runs"`
}

// SetCodePRConfigRequest configures the agent code-PR feature. RunnerToken is
// applied only when non-empty (so saving without re-entering keeps the stored
// one); ClearToken removes it. EgressAllowlist is the set of hosts the runner
// may reach (default-deny otherwise). OutOfScopePolicy is "flag_open" | "pause".
// Daily budgets are workspace/channel caps in MINUTES and runs (0 = unlimited);
// the business layer validates + clamps. WallMinutes is how long ONE coding run
// may work (0 = use the built-in default); a non-zero value outside the code-PR
// safe range is rejected with a clear message rather than silently accepted. An
// empty RunnerURL while Enabled is true is rejected (the tool would have nowhere
// to run).
type SetCodePRConfigRequest struct {
	Enabled               bool     `json:"enabled"`
	RunnerURL             string   `json:"runner_url"`
	RunnerToken           string   `json:"runner_token,omitempty"`
	ClearToken            bool     `json:"clear_token,omitempty"`
	EgressAllowlist       []string `json:"egress_allowlist"`
	OutOfScopePolicy      string   `json:"out_of_scope_policy"`
	DraftOnRed            bool     `json:"draft_on_red"`
	AllowUnlinked         bool     `json:"allow_unlinked"`
	WallMinutes           int      `json:"wall_minutes"`
	WorkspaceDailyMinutes int      `json:"workspace_daily_minutes"`
	WorkspaceDailyRuns    int      `json:"workspace_daily_runs"`
	ChannelDailyMinutes   int      `json:"channel_daily_minutes"`
	ChannelDailyRuns      int      `json:"channel_daily_runs"`
}

// SetRateLimitRequest updates the per-user per-minute ceiling.
type SetRateLimitRequest struct {
	RateLimitPerMin int `json:"rate_limit_per_min"`
}

// SandboxTestResult reports the outcome of an admin "run sample analysis"
// self-test: whether the configured code-runner sidecar is reachable and can
// execute a trivial job under the enforced limits. Ok is true only when the
// sidecar ran the probe to completion; otherwise Message explains why.
type SandboxTestResult struct {
	Ok      bool   `json:"ok"`
	Status  string `json:"status"`
	Message string `json:"message"`
	WallMS  int64  `json:"wall_ms"`
}

// CodePRTestResult reports the code-PR coding-runner deployment self-test:
// whether the configured runner endpoint is reachable and its auth is set. Ok is
// true when the runner answered its liveness probe. (Full clone/edit/push
// validation only happens on a real run — this confirms the deployment is wired.)
type CodePRTestResult struct {
	Ok         bool   `json:"ok"`
	Status     string `json:"status"` // ok | unconfigured | unreachable | error
	Message    string `json:"message"`
	LatencyMS  int64  `json:"latency_ms"`
	TokenSet   bool   `json:"token_set"`
	EndpointOK bool   `json:"endpoint_ok"`
}

// CodePRRunView is one row of the code-PR run ledger for the admin runs list: a
// compact, transparent record of a coding run and its terminal state / PR.
type CodePRRunView struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"` // owner/name
	Status    string `json:"status"`
	Outcome   string `json:"outcome,omitempty"` // merged | closed_unmerged | ""
	PRURL     string `json:"pr_url,omitempty"`
	Draft     bool   `json:"draft"`
	AllPassed bool   `json:"all_passed"`
	DiffFiles int    `json:"diff_files"`
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"created_at"`
}

// SetCodeAnalysisMaxFilesRequest sets the code-agent per-analysis file budget
// (0 = use the default).
type SetCodeAnalysisMaxFilesRequest struct {
	MaxFiles int `json:"max_files"`
}

// SetContextWindowRequest updates the model context window in tokens.
// 0 means "use the env/default" (OLLAMA_NUM_CTX or the built-in 8192).
type SetContextWindowRequest struct {
	ContextWindowTokens int `json:"context_window_tokens"`
}

// SetTokenBudgetRequest updates one of the daily AI token caps (tokens).
// 0 means unlimited. Used for both the workspace and per-user caps.
type SetTokenBudgetRequest struct {
	Tokens int `json:"tokens"`
}

// SetReasoningRequest toggles "thinking" mode for reasoning-capable models.
type SetReasoningRequest struct {
	Enabled bool `json:"enabled"`
}

// SetPIIPatternsRequest sets the admin-defined PII redaction regexes (newline
// delimited, one per line).
type SetPIIPatternsRequest struct {
	Patterns string `json:"patterns"`
}

// TestConnectionRequest probes a provider connection (optionally before
// saving). If ProviderID is set, the stored provider is tested; otherwise
// the inline kind/base_url/api_key are tested.
type TestConnectionRequest struct {
	ProviderID  string `json:"provider_id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	InsecureTLS bool   `json:"insecure_tls,omitempty"`
}

// TestConnectionResponse reports reachability and the discovered catalog.
type TestConnectionResponse struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message"`
	Models  []ModelView `json:"models,omitempty"`
}

// PullModelRequest installs a model on a (local) provider.
type PullModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// DeleteModelRequest removes a locally-installed model.
type DeleteModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

// CatalogModelView is one curated, installable Ollama model, annotated with
// live install state and server-resource feasibility for the admin browser.
type CatalogModelView struct {
	Tag          string   `json:"tag"`
	Family       string   `json:"family"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description"`
	Parameters   string   `json:"parameters"`
	SizeBytes    int64    `json:"size_bytes"`
	MinRAMBytes  int64    `json:"min_ram_bytes"`
	Capabilities []string `json:"capabilities"`
	Recommended  bool     `json:"recommended"`

	// Annotations (live, per request).
	Installed bool `json:"installed"`
	// Feasibility vs the server's actual resources:
	//   "ok"    — comfortably fits
	//   "tight" — runnable but close to the RAM/disk limit
	//   "risky" — likely won't run / not enough disk to download
	//   ""      — unknown (stats unavailable)
	Fit string `json:"fit,omitempty"`
	// FitReason is a short human explanation when Fit is "tight"/"risky".
	FitReason string `json:"fit_reason,omitempty"`
}

// OllamaCatalogResponse is the curated catalog for a local Ollama provider.
type OllamaCatalogResponse struct {
	ProviderID string             `json:"provider_id"`
	Models     []CatalogModelView `json:"models"`
}

// SystemStatsResponse reports server resources for model feasibility.
type SystemStatsResponse struct {
	DiskPath        string   `json:"disk_path"`
	DiskTotalBytes  uint64   `json:"disk_total_bytes"`
	DiskFreeBytes   uint64   `json:"disk_free_bytes"`
	DiskUsedPercent float64  `json:"disk_used_percent"`
	MemTotalBytes   uint64   `json:"mem_total_bytes"`
	MemFreeBytes    uint64   `json:"mem_available_bytes"`
	MemUsedPercent  float64  `json:"mem_used_percent"`
	CPUCount        int      `json:"cpu_count"`
	CPUUsedPercent  float64  `json:"cpu_used_percent,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`

	// Ollama runtime version awareness (only populated when a local
	// Ollama provider is configured).
	OllamaVersion         string `json:"ollama_version,omitempty"`
	OllamaLatestVersion   string `json:"ollama_latest_version,omitempty"`
	OllamaUpdateAvailable bool   `json:"ollama_update_available,omitempty"`
}

// ─── Authorized models (allowlist) ─────────────────────────────────────

// AuthorizeModelRequest adds (or re-enables) a model in the admin allowlist.
type AuthorizeModelRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Label      string `json:"label,omitempty"`
}

// SetModelLimitsRequest records what an admin knows about one model's token limits.
//
// Both are pointers so the handler can tell "field omitted" from "field set to 0", and
// 0 is meaningful here: it clears the value back to inheriting the workspace window. A
// plain int would make clearing indistinguishable from not mentioning the field.
type SetModelLimitsRequest struct {
	ContextWindowTokens *int `json:"context_window_tokens"`
	MaxOutputTokens     *int `json:"max_output_tokens"`
}

// SetEnabledToggleRequest is a shared {enabled bool} body for toggle handlers.
type SetEnabledToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// SetUserModelPreferenceRequest sets a member's chosen model. An empty/omitted
// model_id clears the preference (revert to the workspace default).
type SetUserModelPreferenceRequest struct {
	ModelID string `json:"model_id"`
}

// RunSelfTestRequest optionally targets a specific authorized model for the
// admin AI self-test. Empty model_id tests the workspace default.
type RunSelfTestRequest struct {
	ModelID string `json:"model_id,omitempty"`
}

// UserModelOption is one entry in the member-facing model picker.
type UserModelOption struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Label        string `json:"label"`
	ProviderName string `json:"provider_label"`
	ProviderKind string `json:"provider_kind"`
}

// SetAgentDelegationRequest is the admin agent-to-agent delegation policy. The
// three values travel together because they are one policy: enabling delegation
// with a stale surface list would open places the admin did not just choose.
type SetAgentDelegationRequest struct {
	Enabled  bool   `json:"enabled"`
	MaxHops  int    `json:"max_hops"`
	Surfaces string `json:"surfaces"`
}

// SetMCPServerRequest is the admin admission-control policy for the governed MCP
// surface. Both values travel together because they are one decision: saving the flag
// without the groups enables a surface exposing nothing, and saving groups without the
// flag looks like it took effect when nothing changed.
type SetMCPServerRequest struct {
	Enabled bool `json:"enabled"`
	// ToolGroups is a comma-separated allowlist of scope prefixes ("tasks", "docs",
	// ...) or "*" for all. Empty exposes nothing.
	ToolGroups string `json:"tool_groups"`
}
