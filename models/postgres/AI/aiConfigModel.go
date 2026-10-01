package models

// Database access for the admin-managed AI configuration introduced in
// migration 64 (ai_providers + ai_settings).
//
// OneCamp is single-tenant, so ai_settings is a singleton row (id = 1)
// and ai_providers is a small global set (3 built-ins + any custom
// OpenAI-compatible endpoints the admin adds).
//
// API keys never leave this package in encrypted form: SaveProvider
// encrypts before writing, and the read helpers decrypt into the
// in-memory struct. Controllers receive the decrypted key only when
// they explicitly need it (e.g. to build a provider client); the FE
// is never sent the key — only a HasAPIKey boolean.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Provider kinds — keep aligned with the CHECK constraint in migration 64.
const (
	KindOllama           = "ollama"
	KindOpenAI           = "openai"
	KindAnthropic        = "anthropic"
	KindOpenAICompatible = "openai_compatible"
)

// Fixed UUIDs of the seeded built-in providers (see migration 64).
var (
	BuiltinOllamaID    = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	BuiltinOpenAIID    = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	BuiltinAnthropicID = uuid.MustParse("00000000-0000-0000-0000-0000000000a3")
)

var ErrProviderNotFound = errors.New("ai provider not found")

// ErrProviderKeyUnreadable means a provider has an API key stored that will not decrypt.
//
// A SENTINEL, not a formatted string, so callers can recognise the condition instead of matching on
// message text. It is the difference between two very different answers to an admin: "the provider
// is unreachable" (nothing you can do here) and "re-enter this key" (one field, and you are done).
// ListProviderModels used to answer 502 Bad Gateway for this, which told an admin the upstream was
// broken when nothing had been contacted at all.
//
// IT LIVES HERE, in the model layer, because this is where the condition is DETECTED — scanProvider
// sets KeyUnreadable when the decrypt fails — and because it is the only layer both callers can
// import. business/AI holds the admin surfaces and services/AI resolves the configured endpoint at
// startup; business/AI imports services/AI, so services/AI cannot import back. Defining it in
// either one meant the other had to hand-write its own wording for the same condition, which is
// exactly what had happened: two texts, one of them assembled so that it ended ": groq".
//
// Its message is written FOR AN ADMIN and is safe to show, which is why the controller puts it in
// the response's msg rather than err — helpers.RedactErrorsInResponse deliberately blanks err, and
// the frontend deliberately renders only msg.
//
// Mirrors AIMCP.ErrAuthSecretUnreadable so the same operator condition reads the same way wherever
// it surfaces. Both have one cause: AI_CONFIG_KEK changed, and every secret stored under the old
// value is now undecryptable by design.
//
// Phrased WITHOUT a leading subject ("the stored API key...", not "this provider's stored API
// key...") so that ProviderKeyUnreadableError can name the provider in front of it and the result
// is one sentence rather than two clauses stapled together.
var ErrProviderKeyUnreadable = errors.New("the stored API key can no longer be decrypted " +
	"(usually because AI_CONFIG_KEK changed); re-enter the key in admin AI settings")

// ProviderKeyUnreadableError returns ErrProviderKeyUnreadable named for a specific provider.
//
// Use this rather than composing the wrapper by hand. Every surface that reports the condition —
// the admin model list, the connection test, the startup endpoint resolution — then produces the
// same sentence, and errors.Is(err, ErrProviderKeyUnreadable) keeps working through the wrap.
//
// The label goes in FRONT because it is the subject: "provider groq: the stored API key can no
// longer be decrypted...". Wrapping the other way round, as fmt.Errorf("%w: %s", err, label), reads
// as "...re-enter the key in admin AI settings: groq", which is what this replaces.
//
// An empty label yields the bare sentinel instead of "provider : ...". Labels are NOT NULL in
// ai_providers so this should not arise, but a message is the wrong place to assert that.
func ProviderKeyUnreadableError(label string) error {
	if label == "" {
		return ErrProviderKeyUnreadable
	}
	return fmt.Errorf("provider %s: %w", label, ErrProviderKeyUnreadable)
}

// AIProvider is the decrypted, in-memory form of an ai_providers row.
// APIKey is the plaintext key (empty if none); callers MUST NOT log it.
type AIProvider struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	BaseURL   string    `json:"base_url"`
	APIKey    string    `json:"-"` // decrypted; never serialized to the FE
	HasAPIKey bool      `json:"has_api_key"`
	// KeyUnreadable means a key IS stored but could not be decrypted, so it cannot be used and
	// must be re-entered. Distinct from HasAPIKey=false, which means none was ever set — the
	// admin needs to know the difference, because the usual cause is an AI_CONFIG_KEK rotation
	// rather than anything they did wrong.
	KeyUnreadable bool      `json:"key_unreadable,omitempty"`
	Enabled       bool      `json:"enabled"`
	IsBuiltin     bool      `json:"is_builtin"`
	InsecureTLS   bool      `json:"insecure_tls"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// AISettings is the singleton ai_settings row.
type AISettings struct {
	Enabled             bool       `json:"enabled"`
	ChatProviderID      *uuid.UUID `json:"chat_provider_id"`
	ChatModel           string     `json:"chat_model"`
	EmbeddingProviderID *uuid.UUID `json:"embedding_provider_id"`
	EmbeddingModel      string     `json:"embedding_model"`
	EmbeddingDimension  int        `json:"embedding_dimension"`
	// VisionProviderID/VisionModel select the OPTIONAL multimodal model used
	// to analyze images/GIFs. Empty/nil = vision disabled (image analysis is
	// then unavailable; everything else is unaffected).
	VisionProviderID *uuid.UUID `json:"vision_provider_id"`
	VisionModel      string     `json:"vision_model"`
	RateLimitPerMin  int        `json:"rate_limit_per_min"`
	// ContextWindowTokens is the admin-set model context window in tokens.
	// 0 means "use the env/default" (OLLAMA_NUM_CTX or the built-in 8192).
	// Flows into both the prompt token budget and the provider's num_ctx.
	ContextWindowTokens int `json:"context_window_tokens"`
	// WorkspaceDailyTokenBudget / UserDailyTokenBudget cap daily AI token
	// spend across the whole workspace and per individual user respectively.
	// 0 = unlimited. Enforced at the provider layer so they bound EVERY AI
	// feature. Admin-managed; hot-reloaded with the rest of the config.
	WorkspaceDailyTokenBudget int `json:"workspace_daily_token_budget"`
	UserDailyTokenBudget      int `json:"user_daily_token_budget"`
	// ReasoningEnabled turns on "thinking" mode for reasoning-capable models
	// (gemma4, deepseek-r1, qwen3, …). false (default) = thinking off (faster);
	// true = allow the model's chain-of-thought (often better answers, much
	// slower on CPU). Ignored by non-thinking models.
	ReasoningEnabled    bool `json:"reasoning_enabled"`
	MeetingRecapEnabled bool `json:"meeting_recap_enabled"`
	// MeetingNotesDocEnabled additionally writes the recap and the full
	// transcript into a document. Separate from the recap toggle because it is a
	// different promise: a message is a notification, a document is a thing
	// people own, edit and delete.
	MeetingNotesDocEnabled bool `json:"meeting_notes_doc_enabled"`
	// MeetingRecapInstructions is optional admin free-text guidance appended to
	// the base recap prompt (e.g. "always add a Risks section", "write in
	// Spanish"). Empty = default recap behavior.
	MeetingRecapInstructions string `json:"meeting_recap_instructions"`
	MemoryLayerEnabled       bool   `json:"memory_layer_enabled"`
	TeamReportEnabled        bool   `json:"team_report_enabled"`
	NudgesEnabled            bool   `json:"nudges_enabled"`
	// CoworkerEnabled controls the @mention AI coworker: when a user
	// @mentions the automation bot in a channel, the bot replies in that
	// channel with a channel-scoped answer. Defaults true (it only ever acts
	// on an explicit mention), but an admin can disable it independently of
	// AI chat.
	CoworkerEnabled bool `json:"coworker_enabled"`
	// CodeAnalysisMaxFiles caps how many repo files the code-aware bug agent
	// fetches per analysis (the main cost/latency lever). 0 means "use the
	// built-in default"; the business layer clamps it to a safe range.
	CodeAnalysisMaxFiles int `json:"code_analysis_max_files"`
	// IssueTriageEnabled turns on auto-analysis of newly-opened GitHub issues
	// (posts the proposed fix as an internal task comment). Opt-in (default
	// false) because it spends an LLM call per opened issue.
	IssueTriageEnabled bool `json:"issue_triage_enabled"`
	// LocalOnlyMode is the data-residency guarantee: when true, the AI provider
	// dial guard refuses any non-local model endpoint. Default false.
	LocalOnlyMode bool `json:"local_only_mode"`

	// Agent-to-agent delegation. Admin-configurable here because it is a
	// governance control like every other setting in this struct; the
	// AI_AGENT_DELEGATION env var is retained as a deployment-level KILL SWITCH
	// that can only turn it off, never on, so infrastructure can forbid what the
	// UI would otherwise allow.
	AgentDelegationEnabled bool `json:"agent_delegation_enabled"`
	// AgentDelegationMaxHops bounds how many agent turns deep a chain may go
	// (1-5, enforced by a DB constraint as well as in code). 2 covers
	// human -> triage -> coder.
	AgentDelegationMaxHops int `json:"agent_delegation_max_hops"`
	// AgentDelegationSurfaces is the allowlist of places collaboration is
	// permitted: comma-separated surface keys ("<channel-uuid>" or
	// "task:<task-uuid>"), or "*" for everywhere. Empty = nowhere, so enabling
	// the feature alone does nothing until a surface is named.
	AgentDelegationSurfaces string `json:"agent_delegation_surfaces"`
	// MCPEnabled exposes the governed MCP surface (/v1/mcp) to external agents.
	// Default false, including on upgrade: whether agents outside the workspace may
	// reach it is a decision an admin should make rather than inherit.
	//
	// Enabling it widens nobody's permissions. A call remains bounded by the token's
	// scopes intersected with its owner's live permission on the object; this only
	// governs whether the door is open at all.
	MCPEnabled bool `json:"mcp_enabled"`
	// MCPToolGroups is the allowlist of tool GROUPS the surface exposes:
	// comma-separated scope prefixes ("tasks", "docs", "messages", ...), or "*" for
	// all. Empty = none, so enabling MCP alone exposes no tools until a group is
	// named.
	//
	// Groups rather than individual tools so the list stays answerable — "may agents
	// read our documents" is a question an admin can decide; a list of thirty tool
	// names is one they decide by enabling everything.
	MCPToolGroups string `json:"mcp_tool_groups"`
	// PIIRedactionEnabled scrubs detected PII from outbound prompts before they
	// reach a non-local (cloud) model. Default false.
	PIIRedactionEnabled bool `json:"pii_redaction_enabled"`
	// PIICustomPatterns holds admin-defined redaction regexes, one per line
	// (newline delimited). Invalid patterns are skipped at compile time.
	PIICustomPatterns string `json:"pii_custom_patterns"`

	// Web search (provider-agnostic): which provider the assistant/agents use
	// to search the web, its base URL, and an on/off switch. The API key is
	// stored encrypted and never serialized; HasWebSearchKey tells the FE one
	// is set. Default disabled.
	WebSearchProvider  string `json:"web_search_provider"`
	WebSearchBaseURL   string `json:"web_search_base_url"`
	WebSearchEnabled   bool   `json:"web_search_enabled"`
	WebSearchAPIKeyEnc []byte `json:"-"`
	HasWebSearchKey    bool   `json:"has_web_search_key"`

	// Agent execution sandbox (see .kiro/specs/agent-code-sandbox). OFF by
	// default; when enabled the runner URL/token point at an isolated
	// code-runner sidecar. The token is stored encrypted and never serialized
	// (HasSandboxRunnerToken tells the FE one is set). Budgets are daily caps
	// (0 = unlimited) at the workspace and per-channel tiers; per-agent caps
	// live on the agent row.
	SandboxEnabled               bool   `json:"sandbox_enabled"`
	SandboxRunnerURL             string `json:"sandbox_runner_url"`
	SandboxRunnerTokenEnc        []byte `json:"-"`
	HasSandboxRunnerToken        bool   `json:"has_sandbox_runner_token"`
	SandboxImageDigest           string `json:"sandbox_image_digest"`
	SandboxWorkspaceDailySeconds int    `json:"sandbox_workspace_daily_seconds"`
	SandboxWorkspaceDailyRuns    int    `json:"sandbox_workspace_daily_runs"`
	SandboxChannelDailySeconds   int    `json:"sandbox_channel_daily_seconds"`
	SandboxChannelDailyRuns      int    `json:"sandbox_channel_daily_runs"`

	// Agent code-PR (see .kiro/specs/agent-code-pr). OFF by default; when
	// enabled the runner URL/token point at a coding-capable code-runner
	// sidecar. The token is stored encrypted and never serialized
	// (HasCodePRRunnerToken tells the FE one is set). EgressAllowlist is a raw
	// JSON array of hosts the runner may reach (default-deny otherwise).
	// OutOfScopePolicy is "flag_open" | "pause". Budgets are daily caps in
	// wall-clock MINUTES + run count (0 = unlimited) at the workspace and
	// per-channel tiers; per-agent caps live on the agent row.
	CodePREnabled               bool   `json:"code_pr_enabled"`
	CodePRRunnerURL             string `json:"code_pr_runner_url"`
	CodePRRunnerTokenEnc        []byte `json:"-"`
	HasCodePRRunnerToken        bool   `json:"has_code_pr_runner_token"`
	CodePREgressAllowlist       string `json:"code_pr_egress_allowlist"` // raw JSON array
	CodePROutOfScopePolicy      string `json:"code_pr_out_of_scope_policy"`
	CodePRDraftOnRed            bool   `json:"code_pr_draft_on_red"`
	CodePRWorkspaceDailyMinutes int    `json:"code_pr_workspace_daily_minutes"`
	CodePRWorkspaceDailyRuns    int    `json:"code_pr_workspace_daily_runs"`
	CodePRChannelDailyMinutes   int    `json:"code_pr_channel_daily_minutes"`
	CodePRChannelDailyRuns      int    `json:"code_pr_channel_daily_runs"`
	// CodePRAllowUnlinked lets the agent open a PR on ANY repo the connected
	// GitHub account can reach (verified per-run), not only repos linked to a
	// project. Default false (linked-only) — the blast-radius-safe default for
	// shared installs. The env var AI_CODE_PR_ALLOW_UNLINKED overrides this at
	// boot for automated deployments.
	CodePRAllowUnlinked bool `json:"code_pr_allow_unlinked"`
	// CodePRWallMinutes caps how long ONE coding run may work before the sandbox
	// stops it and the agent hands back whatever it finished. 0 means "use the
	// built-in default" (the server then honors the AI_CODE_PR_WALL_MINUTES env
	// override, if set, and otherwise its compiled-in default); other values are
	// validated/clamped to the code-PR safe range by the business layer.
	CodePRWallMinutes int `json:"code_pr_wall_minutes"`
	// CodePRChatProviderID/CodePRChatModel select an OPTIONAL dedicated model for
	// the coding runner's edit/verify loop, so code runs don't compete with the
	// chat model's provider quota. Nil/empty = fall back to the chat model.
	CodePRChatProviderID *uuid.UUID `json:"code_pr_chat_provider_id"`
	CodePRChatModel      string     `json:"code_pr_chat_model"`

	UpdatedAt time.Time `json:"updated_at"`
}

// ─── Providers ────────────────────────────────────────────────────────

// ListProviders returns all configured providers ordered builtin-first
// then by label. API keys are decrypted into APIKey.
func ListProviders(ctx context.Context) ([]*AIProvider, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT id, kind, label, base_url, api_key_enc, enabled, is_builtin, insecure_tls, updated_at
		FROM ai_providers
		ORDER BY is_builtin DESC, lower(label) ASC`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q)
	if err != nil {
		return nil, fmt.Errorf("list ai providers: %w", err)
	}
	defer rows.Close()

	var out []*AIProvider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider returns a single provider by id (decrypted).
func GetProvider(ctx context.Context, id uuid.UUID) (*AIProvider, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT id, kind, label, base_url, api_key_enc, enabled, is_builtin, insecure_tls, updated_at
		FROM ai_providers WHERE id = $1`

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, id)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProviderNotFound
	}
	return p, err
}

// CreateCustomProvider inserts a new OpenAI-compatible custom endpoint.
// Built-in providers are seeded by migration and never created here.
func CreateCustomProvider(ctx context.Context, label, baseURL, apiKey string, insecureTLS bool) (*AIProvider, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var keyEnc []byte
	if apiKey != "" {
		enc, err := EncryptAPIKey(apiKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt api key: %w", err)
		}
		keyEnc = enc
	}

	id := uuid.New()
	const q = `
		INSERT INTO ai_providers (id, kind, label, base_url, api_key_enc, enabled, is_builtin, insecure_tls)
		VALUES ($1, $2, $3, $4, $5, true, false, $6)`

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		id, KindOpenAICompatible, label, baseURL, keyEnc, insecureTLS); err != nil {
		return nil, fmt.Errorf("create custom ai provider: %w", err)
	}
	return GetProvider(ctx, id)
}

// UpdateProviderInput carries the editable fields. Pointers distinguish
// "not provided" (nil) from "set to empty". APIKey semantics:
//   - nil          → leave the stored key unchanged
//   - &""          → clear the key
//   - &"sk-..."    → replace the key
type UpdateProviderInput struct {
	Label       *string
	BaseURL     *string
	Enabled     *bool
	APIKey      *string
	InsecureTLS *bool
}

// UpdateProvider applies a partial update to a provider row. The kind
// and is_builtin flag are immutable.
func UpdateProvider(ctx context.Context, id uuid.UUID, in UpdateProviderInput) (*AIProvider, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Build the dynamic SET clause safely with positional args.
	set := []string{}
	args := []any{}
	n := 1
	add := func(frag string, val any) {
		set = append(set, fmt.Sprintf(frag, n))
		args = append(args, val)
		n++
	}

	if in.Label != nil {
		add("label = $%d", *in.Label)
	}
	if in.BaseURL != nil {
		add("base_url = $%d", *in.BaseURL)
	}
	if in.Enabled != nil {
		add("enabled = $%d", *in.Enabled)
	}
	if in.InsecureTLS != nil {
		add("insecure_tls = $%d", *in.InsecureTLS)
	}
	if in.APIKey != nil {
		if *in.APIKey == "" {
			add("api_key_enc = $%d", nil)
		} else {
			enc, err := EncryptAPIKey(*in.APIKey)
			if err != nil {
				return nil, fmt.Errorf("encrypt api key: %w", err)
			}
			add("api_key_enc = $%d", enc)
		}
	}

	if len(set) == 0 {
		return GetProvider(ctx, id) // nothing to change
	}

	clause := set[0]
	for _, s := range set[1:] {
		clause += ", " + s
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE ai_providers SET %s, updated_at = NOW() WHERE id = $%d", clause, n)

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("update ai provider: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrProviderNotFound
	}
	return GetProvider(ctx, id)
}

// DeleteCustomProvider removes a custom (non-builtin) provider. Built-in
// providers cannot be deleted — only disabled. Refuses to delete a
// provider that is currently the active chat or embedding selection.
func DeleteCustomProvider(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `DELETE FROM ai_providers WHERE id = $1 AND is_builtin = false`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, id)
	if err != nil {
		return fmt.Errorf("delete ai provider: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// Either not found or it was a built-in (protected).
		return ErrProviderNotFound
	}
	return nil
}

// ─── Settings ─────────────────────────────────────────────────────────

// GetSettings returns the singleton ai_settings row.
func GetSettings(ctx context.Context) (*AISettings, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT enabled, chat_provider_id, chat_model,
		       embedding_provider_id, embedding_model, embedding_dimension,
		       vision_provider_id, vision_model,
		       rate_limit_per_min, context_window_tokens, reasoning_enabled,
		       meeting_recap_enabled, meeting_notes_doc_enabled, meeting_recap_instructions, memory_layer_enabled, team_report_enabled,
		       nudges_enabled, coworker_enabled, code_analysis_max_files, issue_triage_enabled,
		       workspace_daily_token_budget, user_daily_token_budget, local_only_mode,
		       pii_redaction_enabled, pii_custom_patterns,
		       web_search_provider, web_search_base_url, web_search_enabled, web_search_api_key_enc,
		       sandbox_enabled, sandbox_runner_url, sandbox_runner_token_enc, sandbox_image_digest,
		       sandbox_workspace_daily_seconds, sandbox_workspace_daily_runs,
		       sandbox_channel_daily_seconds, sandbox_channel_daily_runs,
		       code_pr_enabled, code_pr_runner_url, code_pr_runner_token_enc,
		       code_pr_egress_allowlist, code_pr_out_of_scope_policy, code_pr_draft_on_red,
		       code_pr_workspace_daily_minutes, code_pr_workspace_daily_runs,
		       code_pr_channel_daily_minutes, code_pr_channel_daily_runs,
		       code_pr_allow_unlinked, code_pr_wall_minutes,
		       code_pr_chat_provider_id, code_pr_chat_model,
		       agent_delegation_enabled, agent_delegation_max_hops, agent_delegation_surfaces,
		       mcp_enabled, mcp_tool_groups,
		       updated_at
		FROM ai_settings WHERE id = 1`

	var s AISettings
	var chatPID, embPID, visionPID, codePRPID uuid.NullUUID
	var updatedAt sql.NullTime
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q).Scan(
		&s.Enabled, &chatPID, &s.ChatModel,
		&embPID, &s.EmbeddingModel, &s.EmbeddingDimension,
		&visionPID, &s.VisionModel,
		&s.RateLimitPerMin, &s.ContextWindowTokens, &s.ReasoningEnabled,
		&s.MeetingRecapEnabled, &s.MeetingNotesDocEnabled, &s.MeetingRecapInstructions, &s.MemoryLayerEnabled, &s.TeamReportEnabled,
		&s.NudgesEnabled, &s.CoworkerEnabled, &s.CodeAnalysisMaxFiles, &s.IssueTriageEnabled,
		&s.WorkspaceDailyTokenBudget, &s.UserDailyTokenBudget, &s.LocalOnlyMode,
		&s.PIIRedactionEnabled, &s.PIICustomPatterns,
		&s.WebSearchProvider, &s.WebSearchBaseURL, &s.WebSearchEnabled, &s.WebSearchAPIKeyEnc,
		&s.SandboxEnabled, &s.SandboxRunnerURL, &s.SandboxRunnerTokenEnc, &s.SandboxImageDigest,
		&s.SandboxWorkspaceDailySeconds, &s.SandboxWorkspaceDailyRuns,
		&s.SandboxChannelDailySeconds, &s.SandboxChannelDailyRuns,
		&s.CodePREnabled, &s.CodePRRunnerURL, &s.CodePRRunnerTokenEnc,
		&s.CodePREgressAllowlist, &s.CodePROutOfScopePolicy, &s.CodePRDraftOnRed,
		&s.CodePRWorkspaceDailyMinutes, &s.CodePRWorkspaceDailyRuns,
		&s.CodePRChannelDailyMinutes, &s.CodePRChannelDailyRuns,
		&s.CodePRAllowUnlinked, &s.CodePRWallMinutes,
		&codePRPID, &s.CodePRChatModel,
		&s.AgentDelegationEnabled, &s.AgentDelegationMaxHops, &s.AgentDelegationSurfaces,
		&s.MCPEnabled, &s.MCPToolGroups,
		&updatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get ai settings: %w", err)
	}
	s.HasWebSearchKey = len(s.WebSearchAPIKeyEnc) > 0
	s.HasSandboxRunnerToken = len(s.SandboxRunnerTokenEnc) > 0
	s.HasCodePRRunnerToken = len(s.CodePRRunnerTokenEnc) > 0
	if chatPID.Valid {
		s.ChatProviderID = &chatPID.UUID
	}
	if embPID.Valid {
		s.EmbeddingProviderID = &embPID.UUID
	}
	if visionPID.Valid {
		s.VisionProviderID = &visionPID.UUID
	}
	if codePRPID.Valid {
		s.CodePRChatProviderID = &codePRPID.UUID
	}
	if updatedAt.Valid {
		s.UpdatedAt = updatedAt.Time
	}
	return &s, nil
}

// SetChatSelection updates the active chat provider+model.
func SetChatSelection(ctx context.Context, providerID uuid.UUID, model string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		UPDATE ai_settings
		SET chat_provider_id = $1, chat_model = $2, updated_at = NOW()
		WHERE id = 1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, providerID, model)
	if err != nil {
		return fmt.Errorf("set chat selection: %w", err)
	}
	return nil
}

// SetEmbeddingSelection updates the active embedding provider+model and
// its vector dimension atomically. The dimension MUST match the live
// OpenSearch index; the reindex flow is responsible for keeping them in
// lockstep.
func SetEmbeddingSelection(ctx context.Context, providerID uuid.UUID, model string, dimension int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		UPDATE ai_settings
		SET embedding_provider_id = $1, embedding_model = $2,
		    embedding_dimension = $3, updated_at = NOW()
		WHERE id = 1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, providerID, model, dimension)
	if err != nil {
		return fmt.Errorf("set embedding selection: %w", err)
	}
	return nil
}

// SetVisionSelection updates (or clears) the optional vision provider+model
// used for image analysis. A nil providerID or empty model CLEARS the
// selection (vision disabled).
func SetVisionSelection(ctx context.Context, providerID *uuid.UUID, model string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if providerID == nil || model == "" {
		const clr = `
			UPDATE ai_settings
			SET vision_provider_id = NULL, vision_model = '', updated_at = NOW()
			WHERE id = 1`
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, clr); err != nil {
			return fmt.Errorf("clear vision selection: %w", err)
		}
		return nil
	}

	const q = `
		UPDATE ai_settings
		SET vision_provider_id = $1, vision_model = $2, updated_at = NOW()
		WHERE id = 1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, *providerID, model); err != nil {
		return fmt.Errorf("set vision selection: %w", err)
	}
	return nil
}

// SetCodePRModelSelection updates (or clears) the OPTIONAL dedicated model the
// coding runner uses for its edit/verify loop. A nil providerID or empty model
// CLEARS the selection (code runs then fall back to the chat model).
func SetCodePRModelSelection(ctx context.Context, providerID *uuid.UUID, model string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if providerID == nil || model == "" {
		const clr = `
			UPDATE ai_settings
			SET code_pr_chat_provider_id = NULL, code_pr_chat_model = '', updated_at = NOW()
			WHERE id = 1`
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, clr); err != nil {
			return fmt.Errorf("clear code pr model selection: %w", err)
		}
		return nil
	}
	const q = `
		UPDATE ai_settings
		SET code_pr_chat_provider_id = $1, code_pr_chat_model = $2, updated_at = NOW()
		WHERE id = 1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, *providerID, model); err != nil {
		return fmt.Errorf("set code pr model selection: %w", err)
	}
	return nil
}

// SetEnabled toggles the global AI switch.
func SetEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set ai enabled: %w", err)
	}
	return nil
}

// SetRateLimit updates the per-user per-minute request ceiling.
func SetRateLimit(ctx context.Context, perMin int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET rate_limit_per_min = $1, updated_at = NOW() WHERE id = 1`, perMin)
	if err != nil {
		return fmt.Errorf("set ai rate limit: %w", err)
	}
	return nil
}

// SetContextWindowTokens updates the admin-set model context window (tokens).
// 0 means "use the env/default". Bounds are enforced by the caller
// (business layer) so the model stays a thin persistence layer.
func SetContextWindowTokens(ctx context.Context, tokens int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET context_window_tokens = $1, updated_at = NOW() WHERE id = 1`, tokens)
	if err != nil {
		return fmt.Errorf("set ai context window: %w", err)
	}
	return nil
}

// SetWorkspaceDailyTokenBudget updates the workspace-wide daily AI token cap
// (0 = unlimited). Bounds are enforced by the business layer.
func SetWorkspaceDailyTokenBudget(ctx context.Context, tokens int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET workspace_daily_token_budget = $1, updated_at = NOW() WHERE id = 1`, tokens)
	if err != nil {
		return fmt.Errorf("set ai workspace daily token budget: %w", err)
	}
	return nil
}

// SetUserDailyTokenBudget updates the per-user daily AI token cap
// (0 = unlimited). Bounds are enforced by the business layer.
func SetUserDailyTokenBudget(ctx context.Context, tokens int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET user_daily_token_budget = $1, updated_at = NOW() WHERE id = 1`, tokens)
	if err != nil {
		return fmt.Errorf("set ai user daily token budget: %w", err)
	}
	return nil
}

// SetReasoningEnabled toggles "thinking" mode for reasoning-capable models.
func SetReasoningEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET reasoning_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set reasoning enabled: %w", err)
	}
	return nil
}

// SetLocalOnlyMode toggles the data-residency guarantee (no content to cloud
// models). Hot-reloaded with the rest of the AI config.
func SetLocalOnlyMode(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET local_only_mode = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set local only mode: %w", err)
	}
	return nil
}

// SetPIIRedactionEnabled toggles PII redaction before cloud egress.
func SetPIIRedactionEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET pii_redaction_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set pii redaction enabled: %w", err)
	}
	return nil
}

// SetPIICustomPatterns stores the admin-defined redaction regexes (newline
// delimited, one per line). Validation/normalization is the caller's job.
func SetPIICustomPatterns(ctx context.Context, patterns string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET pii_custom_patterns = $1, updated_at = NOW() WHERE id = 1`, patterns)
	if err != nil {
		return fmt.Errorf("set pii custom patterns: %w", err)
	}
	return nil
}

// SetWebSearch stores the provider-agnostic web-search configuration. The API
// key is updated only when updateKey is true (so saving other fields without
// re-entering the key keeps the existing one); a nil apiKeyEnc with
// updateKey=true clears it. Caller passes an already-encrypted key blob.
func SetWebSearch(ctx context.Context, provider, baseURL string, enabled bool, apiKeyEnc []byte, updateKey bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if updateKey {
		_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
			`UPDATE ai_settings SET web_search_provider = $1, web_search_base_url = $2,
			        web_search_enabled = $3, web_search_api_key_enc = $4, updated_at = NOW()
			 WHERE id = 1`, provider, baseURL, enabled, apiKeyEnc)
		if err != nil {
			return fmt.Errorf("set web search: %w", err)
		}
		return nil
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET web_search_provider = $1, web_search_base_url = $2,
		        web_search_enabled = $3, updated_at = NOW()
		 WHERE id = 1`, provider, baseURL, enabled)
	if err != nil {
		return fmt.Errorf("set web search: %w", err)
	}
	return nil
}

// SetSandboxConfig stores the agent execution-sandbox configuration. The runner
// token is updated only when updateToken is true (so saving other fields
// without re-entering the token keeps the existing one); a nil tokenEnc with
// updateToken=true clears it. Caller passes an already-encrypted token blob.
// Budget values are bounds-checked by the business layer.
func SetSandboxConfig(ctx context.Context, enabled bool, runnerURL, imageDigest string,
	wsSeconds, wsRuns, chSeconds, chRuns int, tokenEnc []byte, updateToken bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if updateToken {
		const q = `
			UPDATE ai_settings SET sandbox_enabled = $1, sandbox_runner_url = $2,
			       sandbox_image_digest = $3, sandbox_workspace_daily_seconds = $4,
			       sandbox_workspace_daily_runs = $5, sandbox_channel_daily_seconds = $6,
			       sandbox_channel_daily_runs = $7, sandbox_runner_token_enc = $8, updated_at = NOW()
			WHERE id = 1`
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
			enabled, runnerURL, imageDigest, wsSeconds, wsRuns, chSeconds, chRuns, tokenEnc); err != nil {
			return fmt.Errorf("set sandbox config: %w", err)
		}
		return nil
	}
	const q = `
		UPDATE ai_settings SET sandbox_enabled = $1, sandbox_runner_url = $2,
		       sandbox_image_digest = $3, sandbox_workspace_daily_seconds = $4,
		       sandbox_workspace_daily_runs = $5, sandbox_channel_daily_seconds = $6,
		       sandbox_channel_daily_runs = $7, updated_at = NOW()
		WHERE id = 1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		enabled, runnerURL, imageDigest, wsSeconds, wsRuns, chSeconds, chRuns); err != nil {
		return fmt.Errorf("set sandbox config: %w", err)
	}
	return nil
}

// SetSandboxEnabled is the instant kill switch: toggles only the sandbox master
// enable flag without touching runner config or budgets.
func SetSandboxEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET sandbox_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set sandbox enabled: %w", err)
	}
	return nil
}

// SetCodePRConfig stores the agent code-PR configuration: the coding-capable
// code-runner sidecar URL, its auth token (encrypted; updated only when
// updateToken is true — a nil tokenEnc with updateToken=true clears it), the
// egress allowlist (raw JSON array), the out-of-scope policy, the draft-on-red
// flag, the per-run coding wall limit in minutes (0 = use the built-in default),
// and the workspace/channel daily budgets (minutes + runs). Caller passes an
// already-encrypted token blob and validated/clamped values.
func SetCodePRConfig(ctx context.Context, enabled bool, runnerURL, egressAllowlistJSON, outOfScopePolicy string,
	draftOnRed, allowUnlinked bool, wallMinutes, wsMinutes, wsRuns, chMinutes, chRuns int, tokenEnc []byte, updateToken bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(egressAllowlistJSON) == "" {
		egressAllowlistJSON = "[]"
	}
	if updateToken {
		const q = `
			UPDATE ai_settings SET code_pr_enabled = $1, code_pr_runner_url = $2,
			       code_pr_egress_allowlist = $3::jsonb, code_pr_out_of_scope_policy = $4,
			       code_pr_draft_on_red = $5, code_pr_workspace_daily_minutes = $6,
			       code_pr_workspace_daily_runs = $7, code_pr_channel_daily_minutes = $8,
			       code_pr_channel_daily_runs = $9, code_pr_allow_unlinked = $10,
			       code_pr_wall_minutes = $11, code_pr_runner_token_enc = $12, updated_at = NOW()
			WHERE id = 1`
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
			enabled, runnerURL, egressAllowlistJSON, outOfScopePolicy, draftOnRed,
			wsMinutes, wsRuns, chMinutes, chRuns, allowUnlinked, wallMinutes, tokenEnc); err != nil {
			return fmt.Errorf("set code pr config: %w", err)
		}
		return nil
	}
	const q = `
		UPDATE ai_settings SET code_pr_enabled = $1, code_pr_runner_url = $2,
		       code_pr_egress_allowlist = $3::jsonb, code_pr_out_of_scope_policy = $4,
		       code_pr_draft_on_red = $5, code_pr_workspace_daily_minutes = $6,
		       code_pr_workspace_daily_runs = $7, code_pr_channel_daily_minutes = $8,
		       code_pr_channel_daily_runs = $9, code_pr_allow_unlinked = $10,
		       code_pr_wall_minutes = $11, updated_at = NOW()
		WHERE id = 1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		enabled, runnerURL, egressAllowlistJSON, outOfScopePolicy, draftOnRed,
		wsMinutes, wsRuns, chMinutes, chRuns, allowUnlinked, wallMinutes); err != nil {
		return fmt.Errorf("set code pr config: %w", err)
	}
	return nil
}

// SetCodePREnabled is the instant kill switch for the code-PR agent: toggles
// only the master enable flag (runner config and budgets untouched).
func SetCodePREnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET code_pr_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set code pr enabled: %w", err)
	}
	return nil
}

// SetMeetingRecapEnabled toggles the post-call recap ambient agent.
func SetMeetingRecapEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET meeting_recap_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set meeting recap enabled: %w", err)
	}
	return nil
}

// SetMeetingNotesDocEnabled toggles writing the recap and transcript into a
// document. Read live by the recap agent, so no service reload is needed.
func SetMeetingNotesDocEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET meeting_notes_doc_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	return err
}

// SetMeetingRecapInstructions stores the optional admin free-text guidance
// appended to the recap prompt. Trimmed by the caller; an empty string restores
// the default recap behavior.
func SetMeetingRecapInstructions(ctx context.Context, instructions string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET meeting_recap_instructions = $1, updated_at = NOW() WHERE id = 1`, instructions)
	if err != nil {
		return fmt.Errorf("set meeting recap instructions: %w", err)
	}
	return nil
}

// SetMemoryLayerEnabled toggles the workspace-memory extraction agent.
func SetMemoryLayerEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET memory_layer_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set memory layer enabled: %w", err)
	}
	return nil
}

// SetTeamReportEnabled toggles the memory-grounded weekly team-report agent.
func SetTeamReportEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET team_report_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set team report enabled: %w", err)
	}
	return nil
}

// SetNudgesEnabled toggles the proactive-nudges engine.
func SetNudgesEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET nudges_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set nudges enabled: %w", err)
	}
	return nil
}

// SetCoworkerEnabled toggles the @mention AI coworker (channel-scoped
// replies when the automation bot is mentioned).
func SetCoworkerEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET coworker_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set coworker enabled: %w", err)
	}
	return nil
}

// SetCodeAnalysisMaxFiles sets the per-analysis file budget for the code-aware
// bug agent. 0 means "use the default". Bounds are enforced by the caller
// (business layer) so the model stays a thin persistence layer.
func SetCodeAnalysisMaxFiles(ctx context.Context, n int) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET code_analysis_max_files = $1, updated_at = NOW() WHERE id = 1`, n)
	if err != nil {
		return fmt.Errorf("set code analysis max files: %w", err)
	}
	return nil
}

// SetIssueTriageEnabled toggles auto-analysis of newly-opened GitHub issues.
func SetIssueTriageEnabled(ctx context.Context, enabled bool) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings SET issue_triage_enabled = $1, updated_at = NOW() WHERE id = 1`, enabled)
	if err != nil {
		return fmt.Errorf("set issue triage enabled: %w", err)
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────

type scannable interface {
	Scan(dest ...any) error
}

func scanProvider(s scannable) (*AIProvider, error) {
	var p AIProvider
	var keyEnc []byte
	var updatedAt sql.NullTime
	if err := s.Scan(&p.ID, &p.Kind, &p.Label, &p.BaseURL, &keyEnc,
		&p.Enabled, &p.IsBuiltin, &p.InsecureTLS, &updatedAt); err != nil {
		return nil, err
	}
	if updatedAt.Valid {
		p.UpdatedAt = updatedAt.Time
	}
	if len(keyEnc) > 0 {
		key, err := DecryptAPIKey(keyEnc)
		switch {
		case err == nil:
			p.APIKey = key
			p.HasAPIKey = true
		default:
			// AN UNREADABLE KEY IS A RECOVERABLE STATE, NOT A FAILED REQUEST.
			//
			// This used to return the error, which failed the whole of GetAIConfig, which
			// returned 500 for the admin AI settings page. That is a deadlock: the only way to
			// fix an unreadable key is to type a new one on the screen that refuses to load,
			// so a single bad row locked an admin out of the entire AI configuration.
			//
			// It is not hypothetical. Rotating AI_CONFIG_KEK — which is exactly what an
			// operator does on being told the dev fallback is insecure — makes every stored
			// key undecryptable by design, because the key is derived from that value. The
			// expected outcome is "re-enter your provider keys", and that must remain possible.
			//
			// HasAPIKey stays FALSE because there is no usable key: reporting one would tell
			// the admin the provider is configured while every call with it fails.
			// KeyUnreadable carries the distinction the UI needs to say WHY the field is
			// empty, so this is not mistaken for a key that was never set.
			p.KeyUnreadable = true
		}
	}
	return &p, nil
}

// SetAgentDelegation stores the agent-to-agent delegation policy: whether one
// agent may hand work to another, how deep a chain may go, and where it is
// permitted. Hot-reloaded with the rest of the AI config.
//
// The three values are written TOGETHER in one statement rather than as three
// setters, because they are one policy and a partial application is a misleading
// state: enabling delegation with a stale surface list would silently open places
// the admin did not just choose, and saving surfaces without the flag looks like
// it took effect when nothing changed.
//
// maxHops is clamped rather than rejected. The DB constrains it to 1-5, so an
// out-of-range value here is a caller bug, not an operator decision to honour —
// and failing the whole save because a number was too large would lose the
// admin's other two choices. 0 clamps UP to the default because 0 would disable
// what they just enabled.
func SetAgentDelegation(ctx context.Context, enabled bool, maxHops int, surfaces string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if maxHops <= 0 {
		maxHops = 2
	}
	if maxHops > 5 {
		maxHops = 5
	}

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings
		    SET agent_delegation_enabled = $1,
		        agent_delegation_max_hops = $2,
		        agent_delegation_surfaces = $3,
		        updated_at = NOW()
		  WHERE id = 1`,
		enabled, maxHops, strings.TrimSpace(surfaces))
	if err != nil {
		return fmt.Errorf("set agent delegation: %w", err)
	}
	return nil
}

// SetMCPServer writes the MCP admission-control settings.
//
// Both together for the same reason SetAgentDelegation takes its three together: saving
// the flag without the groups enables a surface exposing nothing, and saving groups
// without the flag looks like it took effect when nothing changed. An admin makes one
// decision — "MCP is on, for these areas" — so it is one write.
//
// The group list is stored as given, trimmed. Validated in the business layer rather than
// here: a group name that matches nothing is not a database integrity problem, it is an
// operator mistake worth an error message that names the valid options.
func SetMCPServer(ctx context.Context, enabled bool, toolGroups string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE ai_settings
		    SET mcp_enabled = $1,
		        mcp_tool_groups = $2,
		        updated_at = NOW()
		  WHERE id = 1`,
		enabled, strings.TrimSpace(toolGroups))
	if err != nil {
		return fmt.Errorf("set mcp server: %w", err)
	}
	return nil
}
