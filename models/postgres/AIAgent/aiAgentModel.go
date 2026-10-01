// Package models (AIAgent) is the Postgres data-access layer for the Agent
// Builder. An agent is a user-defined, tool-using AI worker (migration 89); the
// runner executes it, this package only persists and reads agents and their
// run history.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Trigger type constants (keep aligned with the migration CHECK).
const (
	TriggerManual   = "manual"
	TriggerMention  = "mention"
	TriggerSchedule = "schedule"
	TriggerEvent    = "event"
)

// ValidTriggerType reports whether t is a supported trigger kind.
func ValidTriggerType(t string) bool {
	switch t {
	case TriggerManual, TriggerMention, TriggerSchedule, TriggerEvent:
		return true
	default:
		return false
	}
}

// Run status constants (aligned with the migration CHECK).
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunStopped   = "stopped"
)

// MaxStepsCeiling is the hard upper bound on an agent's per-run step budget,
// enforced here and in the runner regardless of the stored value.
const MaxStepsCeiling = 50

// MaxAgentStateLen caps the per-agent continue-the-work state blob so it can't
// grow unbounded across runs (it is injected into the prompt each run).
const MaxAgentStateLen = 8000

// GetAgentState returns the agent's durable continue-the-work state blob (empty
// when none). Read-only; a missing row is "" so a first run starts clean.
func GetAgentState(ctx context.Context, agentID uuid.UUID) (string, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var state string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT state FROM ai_agent_state WHERE agent_id=$1`, agentID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAgentState Failed err: %+v", err)
		return "", err
	}
	return state, nil
}

// SetAgentState upserts the agent's continue-the-work state blob (capped). An
// empty string clears it. Idempotent.
func SetAgentState(ctx context.Context, agentID uuid.UUID, state string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if len(state) > MaxAgentStateLen {
		state = state[:MaxAgentStateLen]
	}
	const q = `INSERT INTO ai_agent_state (agent_id, state, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (agent_id) DO UPDATE SET state = EXCLUDED.state, updated_at = now()`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, agentID, state); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAgentState Failed err: %+v", err)
		return err
	}
	return nil
}

// Autonomy modes (aligned with the migration 104/107 CHECK). Auto = the agent
// performs writes itself; Approval = each write is proposed for human approval
// (governed autonomy); Plan = the agent proposes its FULL ordered plan as one
// approval, executed step-by-step on approval. Read-only tools always run.
const (
	AutonomyAuto     = "auto"
	AutonomyApproval = "approval"
	AutonomyPlan     = "plan"
)

// ValidAutonomy reports whether a is a supported autonomy mode.
func ValidAutonomy(a string) bool {
	return a == AutonomyAuto || a == AutonomyApproval || a == AutonomyPlan
}

// AiAgent mirrors a row of the ai_agents table. JSON columns are kept raw and
// EnabledToolList is the agent's declared toolset, parsed from the raw JSON column.
//
// LIVES ON THE MODEL because more than one surface has to agree about it. The agent
// runner restricts an in-app run to this list; the MCP surface has to restrict a
// credential bound to this agent to the same list, or an agent limited to two tools in
// the builder could reach every tool its owner's token scope allows. Two parsers would
// be two chances to disagree about what "enabled" means, and the disagreement would
// only ever be visible as an agent doing something nobody granted it.
//
// Total and forgiving: a nil agent, an empty column, or malformed JSON all yield nil.
// nil means "no declared restriction" to every caller, which is the pre-existing
// behaviour for an agent whose tools were never configured — so callers must decide
// what an empty list means for them rather than having it decided here.
func (a *AiAgent) EnabledToolList() []string {
	if a == nil || strings.TrimSpace(a.EnabledTools) == "" {
		return nil
	}
	var tools []string
	if err := json.Unmarshal([]byte(a.EnabledTools), &tools); err != nil {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// AgentScope is the parsed shape of an agent's scope JSON: the channels and projects it
// is limited to acting in. Empty lists mean "wherever its owner can act" — the scope
// NARROWS an agent within its owner's access and never widens it.
type AgentScope struct {
	ChannelIDs []string `json:"channel_ids"`
	ProjectIDs []string `json:"project_ids"`
}

// ScopeConfig parses the agent's scope column.
//
// ON THE MODEL for the same reason as EnabledToolList: it had grown three readers — the
// trigger check, the runner, and now the MCP authorizer, which cannot import either of
// the first two. Three parsers of one column is three chances to disagree about what
// "scoped to #support" means, and the disagreement would only ever surface as an agent
// acting somewhere nobody scoped it to.
//
// Total: a nil agent, a blank column, or malformed JSON all yield empty lists, which
// every reader treats as "unrestricted". That is the pre-existing behaviour for an agent
// whose scope was never configured, and it is why the callers — not this function —
// decide what an empty list permits.
func (a *AiAgent) ScopeConfig() AgentScope {
	var s AgentScope
	if a == nil || strings.TrimSpace(a.Scope) == "" {
		return s
	}
	// A parse failure leaves s zero-valued, which reads as unrestricted. Deliberate and
	// matching what the runner and the trigger check already did: a corrupted scope must
	// not silently confine an agent to nowhere and make it look broken.
	_ = json.Unmarshal([]byte(a.Scope), &s)
	return s
}

// ManageableBy reports whether a person may administer this agent: its owner, or a
// workspace admin.
//
// ON THE MODEL for the same reason as EnabledToolList — a second caller appeared that
// must not import the first. Binding an API credential to an agent identity is an
// administrative act on the agent, so it has to ask the same question the agent
// builder asks, and asking it with a second copy of the rule is how the two drift.
func (a *AiAgent) ManageableBy(userID uuid.UUID, isAdmin bool) bool {
	if a == nil {
		return false
	}
	return isAdmin || a.CreatedBy == userID
}

// parsed/validated in the business layer.
type AiAgent struct {
	Id             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	AvatarKey      *string   `json:"avatar_key,omitempty"`
	Description    *string   `json:"description,omitempty"`
	Instructions   string    `json:"instructions"`
	ModelPref      *string   `json:"model_pref,omitempty"`
	EnabledTools   string    `json:"enabled_tools"` // raw JSON array
	TriggerType    string    `json:"trigger_type"`
	TriggerConfig  string    `json:"trigger_config"` // raw JSON object
	Scope          string    `json:"scope"`          // raw JSON object
	MaxSteps       int       `json:"max_steps"`
	IsActive       bool      `json:"is_active"`
	DmAble         bool      `json:"dm_able"`
	Autonomy       string    `json:"autonomy"`
	Knowledge      string    `json:"knowledge"`        // raw JSON array of {type,id,label}
	SkillIds       string    `json:"skill_ids"`        // raw JSON array of skill uuids
	MaxDailyTokens int       `json:"max_daily_tokens"` // per-agent daily token cap (0 = none)
	// SandboxDailySeconds / SandboxDailyRuns are the per-agent daily execution-
	// sandbox caps (0 = no per-agent cap; the workspace/channel caps still
	// apply). Mirror max_daily_tokens.
	SandboxDailySeconds int `json:"sandbox_daily_seconds"`
	SandboxDailyRuns    int `json:"sandbox_daily_runs"`
	// Ambient: when true, the agent may reply in its scoped channels without an
	// @mention (opt-in, rate-limited, budget-metered, self-selecting).
	// AmbientKeywords narrows candidacy to its topic (comma/newline separated;
	// empty = only questions are considered).
	Ambient         bool   `json:"ambient"`
	AmbientKeywords string `json:"ambient_keywords"`
	// RunInBackground: when true, a channel/DM @mention runs as a durable job
	// (evolving in-thread progress, survives restarts) instead of one sync pass.
	RunInBackground bool `json:"run_in_background"`
	// A remote brain. When AGUIEndpoint is set, the agent's reasoning happens at
	// that AG-UI endpoint and this workspace supplies the tools, the rules and
	// the record. AGUIAuthHeader names the header the secret travels in; empty
	// means Authorization: Bearer. The secret itself is decrypted into
	// AGUIAuthSecret for the runner and never serialised; AGUIAuthSet is what a
	// client is told.
	AGUIEndpoint string `json:"agui_endpoint,omitempty"`
	// RemoteProtocol is how the remote brain is spoken to: "agui" (the
	// default) or "a2a". RemoteCard is what an A2A agent card said about it.
	RemoteProtocol string          `json:"remote_protocol,omitempty"`
	RemoteCard     json.RawMessage `json:"remote_card,omitempty"`
	AGUIAuthHeader string          `json:"agui_auth_header,omitempty"`
	AGUIAuthSecret string          `json:"-"`
	AGUIAuthSet    bool            `json:"agui_auth_set,omitempty"`
	// AGUIAuthUnreadable: a secret is stored but cannot be decrypted (an
	// AI_CONFIG_KEK change), so the agent cannot run until it is re-entered.
	// Distinct from AGUIAuthSet=false, which means none was ever given.
	AGUIAuthUnreadable bool      `json:"agui_auth_unreadable,omitempty"`
	CreatedBy          uuid.UUID `json:"created_by"`
	// CreatedByName is who authorised this agent, filled in by the business layer
	// for lists rather than stored. Empty when the person cannot be resolved, so
	// the interface can say that in its own words instead of showing a uuid.
	CreatedByName string     `json:"created_by_name,omitempty"`
	BotUserId     *uuid.UUID `json:"bot_user_id,omitempty"`
	RunCount      int64      `json:"run_count"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastError     *string    `json:"last_error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// AgentRun mirrors a row of the ai_agent_runs table.
type AgentRun struct {
	Id            uuid.UUID  `json:"id"`
	AgentId       uuid.UUID  `json:"agent_id"`
	TriggerSource string     `json:"trigger_source"`
	RunAsUserId   *uuid.UUID `json:"run_as_user_id,omitempty"`
	Status        string     `json:"status"`
	Steps         string     `json:"steps"` // raw JSON array transcript
	StepCount     int        `json:"step_count"`
	// TriggerPrompt is what the agent was asked. Empty on runs recorded before
	// this was kept, which is why the review surface offers only runs that have
	// one rather than inventing a prompt for the rest.
	TriggerPrompt string     `json:"trigger_prompt,omitempty"`
	Tokens        int64      `json:"tokens"`
	Result        *string    `json:"result,omitempty"`
	Error         *string    `json:"error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	// Model is the model that actually read the prompt, after the agent, channel
	// and workspace precedence was resolved. The agent's stored preference is an
	// input to that decision, not the answer to it.
	Model string `json:"model,omitempty"`
	// PromptSHA256 fingerprints the fully composed system prompt, and SkillsUsed
	// names the skills that were in it with a fingerprint of each one's text at
	// the time. Neither is content, so both outlive the retention sweep that
	// clears the transcript: a redacted run can still say which instructions
	// produced it after it can no longer show them.
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	SkillsUsed   string `json:"skills_used,omitempty"` // raw JSON array

	// RedactedAt is set once retention has cleared this run's transcript.
	//
	// Sent to the client so a cleared run can say so. Without it the UI shows a
	// run that reports twelve steps and displays none, which reads as a bug and
	// sends someone looking for a failure that never happened.
	RedactedAt *time.Time `json:"redacted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const agentColumns = `id, name, avatar_key, description, instructions, model_pref,
	enabled_tools, trigger_type, trigger_config, scope, max_steps, is_active,
	created_by, bot_user_id, run_count, last_run_at, last_error, created_at, updated_at, deleted_at, dm_able, autonomy, knowledge, skill_ids, max_daily_tokens, sandbox_daily_seconds, sandbox_daily_runs, run_in_background, ambient, ambient_keywords,
	agui_endpoint, agui_auth_header, agui_auth_secret_encrypted, remote_protocol, remote_card`

// nullJSON stores an empty document as NULL rather than invalid JSON.
func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func scanAgent(s scanner) (*AiAgent, error) {
	var a AiAgent
	var avatarKey, description, modelPref, lastError sql.NullString
	var remoteCard []byte
	var lastRunAt, deletedAt sql.NullTime
	var botUserID uuid.NullUUID
	var aguiSecret []byte

	err := s.Scan(
		&a.Id,
		&a.Name,
		&avatarKey,
		&description,
		&a.Instructions,
		&modelPref,
		&a.EnabledTools,
		&a.TriggerType,
		&a.TriggerConfig,
		&a.Scope,
		&a.MaxSteps,
		&a.IsActive,
		&a.CreatedBy,
		&botUserID,
		&a.RunCount,
		&lastRunAt,
		&lastError,
		&a.CreatedAt,
		&a.UpdatedAt,
		&deletedAt,
		&a.DmAble,
		&a.Autonomy,
		&a.Knowledge,
		&a.SkillIds,
		&a.MaxDailyTokens,
		&a.SandboxDailySeconds,
		&a.SandboxDailyRuns,
		&a.RunInBackground,
		&a.Ambient,
		&a.AmbientKeywords,
		&a.AGUIEndpoint,
		&a.AGUIAuthHeader,
		&aguiSecret,
		&a.RemoteProtocol,
		&remoteCard,
	)
	if err != nil {
		return nil, err
	}
	a.AGUIAuthSecret, a.AGUIAuthSet, a.AGUIAuthUnreadable = readAGUISecret(a.Id, aguiSecret)
	if len(remoteCard) > 0 {
		a.RemoteCard = json.RawMessage(remoteCard)
	}
	if avatarKey.Valid {
		a.AvatarKey = &avatarKey.String
	}
	if description.Valid {
		a.Description = &description.String
	}
	if modelPref.Valid {
		a.ModelPref = &modelPref.String
	}
	if lastError.Valid {
		a.LastError = &lastError.String
	}
	if botUserID.Valid {
		id := botUserID.UUID
		a.BotUserId = &id
	}
	if lastRunAt.Valid {
		a.LastRunAt = &lastRunAt.Time
	}
	if deletedAt.Valid {
		a.DeletedAt = &deletedAt.Time
	}
	return &a, nil
}

// CreateAgent inserts a new agent and returns the generated id.
func CreateAgent(ctx context.Context, a *AiAgent) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(a.EnabledTools) == "" {
		a.EnabledTools = "[]"
	}
	if strings.TrimSpace(a.TriggerConfig) == "" {
		a.TriggerConfig = "{}"
	}
	if strings.TrimSpace(a.Scope) == "" {
		a.Scope = "{}"
	}
	if !ValidAutonomy(a.Autonomy) {
		a.Autonomy = AutonomyAuto
	}
	if strings.TrimSpace(a.Knowledge) == "" {
		a.Knowledge = "[]"
	}
	if strings.TrimSpace(a.SkillIds) == "" {
		a.SkillIds = "[]"
	}
	id := uuid.New()
	const q = `INSERT INTO ai_agents
		(id, name, avatar_key, description, instructions, model_pref, enabled_tools,
		 trigger_type, trigger_config, scope, max_steps, is_active, created_by, dm_able, autonomy, knowledge, skill_ids, max_daily_tokens, sandbox_daily_seconds, sandbox_daily_runs, run_in_background, ambient, ambient_keywords,
		 agui_endpoint, agui_auth_header, agui_auth_secret_encrypted, remote_protocol, remote_card)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`
	secret, err := encryptAGUISecret(a.AGUIAuthSecret)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateAgent encrypt err: %+v", err)
		return uuid.Nil, err
	}
	_, err = postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, a.Name, a.AvatarKey, a.Description, a.Instructions, a.ModelPref, a.EnabledTools,
		a.TriggerType, a.TriggerConfig, a.Scope, a.MaxSteps, a.IsActive, a.CreatedBy, a.DmAble, a.Autonomy, a.Knowledge, a.SkillIds, a.MaxDailyTokens, a.SandboxDailySeconds, a.SandboxDailyRuns, a.RunInBackground, a.Ambient, a.AmbientKeywords,
		a.AGUIEndpoint, a.AGUIAuthHeader, secret, remoteProtocolOrDefault(a.RemoteProtocol), nullJSON(a.RemoteCard))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateAgent Failed err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateAgent updates the editable fields of an agent.
func UpdateAgent(ctx context.Context, a *AiAgent) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(a.EnabledTools) == "" {
		a.EnabledTools = "[]"
	}
	if strings.TrimSpace(a.TriggerConfig) == "" {
		a.TriggerConfig = "{}"
	}
	if strings.TrimSpace(a.Scope) == "" {
		a.Scope = "{}"
	}
	if !ValidAutonomy(a.Autonomy) {
		a.Autonomy = AutonomyAuto
	}
	if strings.TrimSpace(a.Knowledge) == "" {
		a.Knowledge = "[]"
	}
	if strings.TrimSpace(a.SkillIds) == "" {
		a.SkillIds = "[]"
	}
	const q = `UPDATE ai_agents
		SET name=$2, avatar_key=$3, description=$4, instructions=$5, model_pref=$6,
		    enabled_tools=$7, trigger_type=$8, trigger_config=$9, scope=$10,
		    max_steps=$11, is_active=$12, dm_able=$13, autonomy=$14, knowledge=$15, skill_ids=$16, max_daily_tokens=$17, sandbox_daily_seconds=$18, sandbox_daily_runs=$19, run_in_background=$20, ambient=$21, ambient_keywords=$22,
		    agui_endpoint=$23, agui_auth_header=$24, agui_auth_secret_encrypted=$25, remote_protocol=$26, remote_card=$27, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	secret, err := encryptAGUISecret(a.AGUIAuthSecret)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateAgent encrypt err: %+v", err)
		return err
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		a.Id, a.Name, a.AvatarKey, a.Description, a.Instructions, a.ModelPref, a.EnabledTools,
		a.TriggerType, a.TriggerConfig, a.Scope, a.MaxSteps, a.IsActive, a.DmAble, a.Autonomy, a.Knowledge, a.SkillIds, a.MaxDailyTokens, a.SandboxDailySeconds, a.SandboxDailyRuns, a.RunInBackground, a.Ambient, a.AmbientKeywords,
		a.AGUIEndpoint, a.AGUIAuthHeader, secret, remoteProtocolOrDefault(a.RemoteProtocol), nullJSON(a.RemoteCard))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateAgent Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAgentActive toggles an agent's enabled state.
func SetAgentActive(ctx context.Context, id uuid.UUID, isActive bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_agents SET is_active=$2, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, isActive)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAgentActive Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAgentBotUser persists the agent's resolved bot principal id (denormalized
// link to the per-agent users row). Idempotent; only writes when changed.
func SetAgentBotUser(ctx context.Context, id, botUserID uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_agents SET bot_user_id=$2, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL AND (bot_user_id IS DISTINCT FROM $2)`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, botUserID); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAgentBotUser Failed err: %+v", err)
		return err
	}
	return nil
}

// SetAgentScope persists just the agent's scope JSON (the channels/projects it
// is limited to). Used by the in-channel "AI teammates" control to add/remove a
// channel from an agent's scope without a full update. Idempotent.
func SetAgentScope(ctx context.Context, id uuid.UUID, scopeJSON string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(scopeJSON) == "" {
		scopeJSON = "{}"
	}
	const q = `UPDATE ai_agents SET scope=$2, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, scopeJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAgentScope Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SoftDeleteAgent marks an agent deleted.
func SoftDeleteAgent(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_agents SET deleted_at=NOW(), is_active=false, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteAgent Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetAgentByID returns a single non-deleted agent, or (nil, nil) if absent.
func GetAgentByID(ctx context.Context, id uuid.UUID) (*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents WHERE id=$1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAgentByID Failed err: %+v", err)
		return nil, err
	}
	return a, nil
}

// GetAgentByBotUser returns the live agent whose workspace identity is this bot
// user, or (nil, nil) if there is none. A bot is how members meet an agent (in
// a channel, in a DM), so this is the way from "who is this?" to the agent.
func GetAgentByBotUser(ctx context.Context, botUserID uuid.UUID) (*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents WHERE bot_user_id=$1 AND deleted_at IS NULL LIMIT 1`
	a, err := scanAgent(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, botUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAgentByBotUser Failed err: %+v", err)
		return nil, err
	}
	return a, nil
}

// ListActiveAgentsByBotUsers returns the live, active agents whose workspace
// identity is one of these bot users, by name.
func ListActiveAgentsByBotUsers(ctx context.Context, botUserIDs []uuid.UUID) ([]*AiAgent, error) {
	if len(botUserIDs) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents
		WHERE bot_user_id = ANY($1) AND is_active = true AND deleted_at IS NULL ORDER BY name`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, pq.Array(botUserIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListActiveAgentsByBotUsers Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AiAgent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAgents returns all non-deleted agents, newest first.
func ListAgents(ctx context.Context) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListAgents Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListAgents scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAgentsByCreator returns a single user's non-deleted agents, newest first.
// Used for non-admin members so the filter runs in SQL (served by the
// created_by partial index) instead of fetching all agents and filtering in Go.
func ListAgentsByCreator(ctx context.Context, createdBy uuid.UUID) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents WHERE deleted_at IS NULL AND created_by=$1 ORDER BY created_at DESC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListAgentsByCreator Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListAgentsByCreator scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListDMable returns active, non-deleted agents that are DM-able (Req 10.1) —
// the set whose bot principals can be addressed as a 1:1 DM target and whose
// messages route to that agent's runner. Small set; not on a per-message hot
// path (callers cache it).
func ListDMable(ctx context.Context) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents
		WHERE dm_able=true AND is_active=true AND deleted_at IS NULL ORDER BY name ASC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListDMable Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListDMable scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAmbient returns active, non-deleted agents opted into ambient mode (they
// may reply in their scoped channels without an @mention). Small set; cached by
// the trigger workers, not on the per-message hot path.
func ListAmbient(ctx context.Context) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents
		WHERE ambient=true AND is_active=true AND deleted_at IS NULL ORDER BY name ASC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListAmbient Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListAmbient scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WorkspaceAgentStats is a fleet-level rollup across agents (optionally scoped
// to one creator), powering the admin's agent overview: how many agents exist /
// are active, and the aggregate run health + spend across them. The "is the
// agent fleet healthy and worth the cost" view.
type WorkspaceAgentStats struct {
	TotalAgents  int64 `json:"total_agents"`
	ActiveAgents int64 `json:"active_agents"`
	TotalRuns    int64 `json:"total_runs"`
	Succeeded    int64 `json:"succeeded"`
	Failed       int64 `json:"failed"`
	Stopped      int64 `json:"stopped"`
	Running      int64 `json:"running"`
	TotalTokens  int64 `json:"total_tokens"`
	Last7dRuns   int64 `json:"last_7d_runs"`
	Last7dTokens int64 `json:"last_7d_tokens"`
}

// ExecWorkspaceAgentStats runs the two pre-built aggregate queries (see
// domain/AIAgent, which owns the optional created_by scoping) — agent counts
// and run health — and scans both into the fleet rollup. Runs are joined to
// non-deleted agents so a soft-deleted agent's history does not inflate the
// fleet view.
func ExecWorkspaceAgentStats(ctx context.Context, countQ string, countArgs []interface{}, runQ string, runArgs []interface{}) (*WorkspaceAgentStats, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var s WorkspaceAgentStats

	// 1. Agent counts.
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, countQ, countArgs...).Scan(&s.TotalAgents, &s.ActiveAgents); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecWorkspaceAgentStats counts err: %+v", err)
		return nil, err
	}

	// 2. Run health across those agents.
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, runQ, runArgs...).Scan(
		&s.TotalRuns, &s.Succeeded, &s.Failed, &s.Stopped, &s.Running,
		&s.TotalTokens, &s.Last7dRuns, &s.Last7dTokens); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecWorkspaceAgentStats runs err: %+v", err)
		return nil, err
	}
	return &s, nil
}

// ListActiveWithBotPrincipal returns active, non-deleted agents that already
// have a provisioned bot principal (bot_user_id set). Only such agents can be a
// task assignee / be addressed as a principal, so this is the small candidate
// set for resolving "which agent is this assignee" without provisioning a
// principal for every agent in the workspace.
func ListActiveWithBotPrincipal(ctx context.Context) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents
		WHERE is_active=true AND deleted_at IS NULL AND bot_user_id IS NOT NULL
		ORDER BY name ASC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListActiveWithBotPrincipal Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListActiveWithBotPrincipal scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListActiveByTrigger returns active, non-deleted agents for a trigger type
// (the trigger workers' hot path — backed by idx_ai_agents_active_trigger).
func ListActiveByTrigger(ctx context.Context, triggerType string) ([]*AiAgent, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + agentColumns + ` FROM ai_agents
		WHERE trigger_type=$1 AND is_active=true AND deleted_at IS NULL`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, triggerType)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListActiveByTrigger Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AiAgent
	for rows.Next() {
		a, scanErr := scanAgent(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListActiveByTrigger(agent) scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateRun opens a new run row (status=running) and returns its id.
// CreateRun opens a run row. prompt is what the agent was asked, kept so a run
// that went wrong can become the scenario that catches it next time; a run
// recorded only what it did, which is reviewable but not reusable.
// maxTriggerPromptRunes bounds what is kept. An eval scenario caps its prompt at
// 4000, so storing more than that could never become one anyway.
const maxTriggerPromptRunes = 4000

func CreateRun(ctx context.Context, agentId uuid.UUID, triggerSource string, runAsUserId *uuid.UUID, prompt string) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	id := uuid.New()
	const q = `INSERT INTO ai_agent_runs (id, agent_id, trigger_source, run_as_user_id, trigger_prompt)
		VALUES ($1,$2,$3,$4,$5)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, agentId, triggerSource, runAsUserId,
		nullStr(helpers.TruncateRunes(prompt, maxTriggerPromptRunes)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateRun Failed err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// FinishRun closes a run with its final status, transcript, and bookkeeping,
// and bumps the agent's run counters in the same call.
// RecordRunProvenance stores what the agent was told, as soon as it is known.
//
// Separate from FinishRun rather than another parameter on it, because the two
// answer different questions at different times. What the agent was ASKED is
// settled the moment the prompt is composed; what it DID is only settled at the
// end. Writing it here means a run that crashes, times out or is killed still
// carries the instructions that produced the crash, which is exactly the run
// somebody will want to explain.
//
// Best effort by contract: losing the provenance is bad, and refusing to run the
// agent because a metadata write failed is worse.
func RecordRunProvenance(ctx context.Context, runId uuid.UUID, modelName, promptSHA, skillsUsedJSON string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(skillsUsedJSON) == "" {
		skillsUsedJSON = "[]"
	}
	const q = `UPDATE ai_agent_runs
	              SET model = $2, prompt_sha256 = $3, skills_used = $4::jsonb
	            WHERE id = $1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, runId, modelName, promptSHA, skillsUsedJSON); err != nil {
		helpers.LogErrorWithContext(ctx, "models/RecordRunProvenance err: %+v", err)
		return err
	}
	return nil
}

func FinishRun(ctx context.Context, runId, agentId uuid.UUID, status, stepsJSON string, stepCount int, tokens int64, result, errMsg string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(stepsJSON) == "" {
		stepsJSON = "[]"
	}
	var resultArg, errArg interface{}
	if result != "" {
		resultArg = result
	}
	if errMsg != "" {
		errArg = errMsg
	}
	const q = `UPDATE ai_agent_runs
		SET status=$2, steps=$3, step_count=$4, tokens=$5, result=$6, error=$7, ended_at=NOW()
		WHERE id=$1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, runId, status, stepsJSON, stepCount, tokens, resultArg, errArg); err != nil {
		helpers.LogErrorWithContext(ctx, "models/FinishRun Failed err: %+v", err)
		return err
	}
	const q2 = `UPDATE ai_agents
		SET run_count = run_count + 1, last_run_at = NOW(), last_error = $2, updated_at = NOW()
		WHERE id = $1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q2, agentId, errArg); err != nil {
		helpers.LogErrorWithContext(ctx, "models/FinishRun bookkeeping err: %+v", err)
		return err
	}
	return nil
}

// ClaimDueScheduledRun atomically claims a scheduled agent's due occurrence so
// EXACTLY ONE replica dispatches it. It advances last_run_at to now ONLY IF the
// row's last_run_at still equals the value the caller observed when it decided
// the agent was due (optimistic compare-and-set). On a multi-replica deployment
// this guarantees at-most-once dispatch per occurrence: the in-process
// KeyedLock cannot, since it does not span processes, so without this two
// replicas would both fire a due scheduled agent (duplicate check-ins, doubled
// token spend). Returns true only for the caller that won the claim; the loser
// (and any later tick before the run finishes) sees false and skips.
//
// Stamping at claim time (not only at FinishRun) also makes the interval cadence
// measure from the start of a run, so a slow run cannot re-fire on the next
// tick. observedLastRun may be nil (an agent that has never run); the
// IS NOT DISTINCT FROM comparison handles NULL correctly.
func ClaimDueScheduledRun(ctx context.Context, agentID uuid.UUID, observedLastRun *time.Time) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var obs interface{}
	if observedLastRun != nil {
		obs = observedLastRun.UTC()
	}
	const q = `UPDATE ai_agents
		SET last_run_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL AND last_run_at IS NOT DISTINCT FROM $2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, agentID, obs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueScheduledRun err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentRunStats is an at-a-glance reliability + activity rollup for one agent,
// aggregated over its run history (ai_agent_runs). It is the data behind the
// agent's reliability panel: how often it succeeds, how much it spends, how
// long it takes, and how active it is — the "can I trust this running
// autonomously / is it worth the cost" view.
type AgentRunStats struct {
	TotalRuns     int64      `json:"total_runs"`
	Succeeded     int64      `json:"succeeded"`
	Failed        int64      `json:"failed"`
	Stopped       int64      `json:"stopped"`
	Running       int64      `json:"running"`
	TotalTokens   int64      `json:"total_tokens"`
	AvgSteps      float64    `json:"avg_steps"`
	AvgDurationMs float64    `json:"avg_duration_ms"`
	Last7dRuns    int64      `json:"last_7d_runs"`
	Last7dTokens  int64      `json:"last_7d_tokens"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	// TokensToday / MaxDailyTokens surface the agent's per-agent daily budget:
	// how much it has spent today and its configured cap (0 = no cap). Populated
	// by the business layer (not from the run-history aggregate), so the panel
	// can show "AI today: used / cap".
	TokensToday    int64 `json:"tokens_today"`
	MaxDailyTokens int   `json:"max_daily_tokens"`
	// Sandbox*Today / SandboxDaily* mirror the token budget for the agent
	// execution sandbox: today's runs + runner-seconds and the per-agent daily
	// caps (0 = no cap). Populated by the business layer only when the sandbox
	// feature is enabled, so the panel can show "Sandbox today: runs / cap".
	SandboxRunsToday    int `json:"sandbox_runs_today"`
	SandboxSecondsToday int `json:"sandbox_seconds_today"`
	SandboxDailyRuns    int `json:"sandbox_daily_runs"`
	SandboxDailySeconds int `json:"sandbox_daily_seconds"`
	// WorkingNotes is the agent's durable continue-the-work state (what it is
	// carrying forward across runs), surfaced read-only for transparency.
	WorkingNotes string `json:"working_notes,omitempty"`
}

// GetAgentRunStats computes the reliability/activity rollup for an agent in a
// single aggregate query (Postgres FILTER + EXTRACT), so the panel is cheap to
// render. Duration is averaged only over completed runs (ended_at set).
func GetAgentRunStats(ctx context.Context, agentId uuid.UUID) (*AgentRunStats, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT
		COUNT(*) AS total,
		COUNT(*) FILTER (WHERE status='succeeded') AS succeeded,
		COUNT(*) FILTER (WHERE status='failed') AS failed,
		COUNT(*) FILTER (WHERE status='stopped') AS stopped,
		COUNT(*) FILTER (WHERE status='running') AS running,
		COALESCE(SUM(tokens),0) AS total_tokens,
		COALESCE(AVG(step_count),0) AS avg_steps,
		COALESCE(AVG(EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000)
			FILTER (WHERE ended_at IS NOT NULL),0) AS avg_duration_ms,
		COUNT(*) FILTER (WHERE started_at > NOW() - INTERVAL '7 days') AS last7d_runs,
		COALESCE(SUM(tokens) FILTER (WHERE started_at > NOW() - INTERVAL '7 days'),0) AS last7d_tokens,
		MAX(started_at) AS last_run_at
		FROM ai_agent_runs WHERE agent_id=$1`

	var s AgentRunStats
	var lastRunAt sql.NullTime
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, agentId)
	if err := row.Scan(&s.TotalRuns, &s.Succeeded, &s.Failed, &s.Stopped, &s.Running,
		&s.TotalTokens, &s.AvgSteps, &s.AvgDurationMs, &s.Last7dRuns, &s.Last7dTokens, &lastRunAt); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAgentRunStats Failed err: %+v", err)
		return nil, err
	}
	if lastRunAt.Valid {
		s.LastRunAt = &lastRunAt.Time
	}
	return &s, nil
}

// AgentHealth is a compact per-agent reliability signal for the agents list:
// just enough to render an at-a-glance status dot + tooltip per row, without
// the full stats panel. Success rate is computed by the caller over the
// terminal (completed) runs.
type AgentHealth struct {
	AgentID    uuid.UUID  `json:"agent_id"`
	TotalRuns  int64      `json:"total_runs"`
	Succeeded  int64      `json:"succeeded"`
	Failed     int64      `json:"failed"`
	Stopped    int64      `json:"stopped"`
	Running    int64      `json:"running"`
	Last7dRuns int64      `json:"last_7d_runs"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
}

// ExecAgentHealthBatch runs a pre-built per-agent health rollup query (see
// domain/AIAgent, which owns the optional created_by scoping) and scans it into
// a map keyed by agent id. The query is a SINGLE grouped read so the agents
// list shows a per-row status dot without an N+1 of GetAgentRunStats. Only
// agents with at least one run appear; the FE treats a missing entry as "no
// runs yet".
func ExecAgentHealthBatch(ctx context.Context, query string, args []interface{}) (map[string]*AgentHealth, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecAgentHealthBatch Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]*AgentHealth)
	for rows.Next() {
		var h AgentHealth
		var lastRunAt sql.NullTime
		if err := rows.Scan(&h.AgentID, &h.TotalRuns, &h.Succeeded, &h.Failed, &h.Stopped, &h.Running, &h.Last7dRuns, &lastRunAt); err != nil {
			helpers.LogErrorWithContext(ctx, "models/ExecAgentHealthBatch scan Failed err: %+v", err)
			return nil, err
		}
		if lastRunAt.Valid {
			h.LastRunAt = &lastRunAt.Time
		}
		out[h.AgentID.String()] = &h
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecAgentHealthBatch rows err: %+v", err)
		return nil, err
	}
	return out, nil
}

// ListRunsByAgent returns the most recent runs for an agent (run history).
// EvidenceRun is a run as it appears in an evidence pack: what was asked, what
// was told, what happened, and nothing else.
//
// Deliberately NOT the full AgentRun. The transcript is the largest and most
// sensitive field in the product and an evidence pack is a document that leaves
// the building, so it carries the FINGERPRINT of the instructions rather than
// the instructions, and the counts rather than the conversation. A reviewer
// checking whether an edit to a shared skill changed what an agent was asked
// needs the digest; they do not need to read the workspace's messages.
type EvidenceRun struct {
	Id            uuid.UUID  `json:"id"`
	AgentId       uuid.UUID  `json:"agent_id"`
	TriggerSource string     `json:"trigger_source"`
	Status        string     `json:"status"`
	StepCount     int        `json:"step_count"`
	Tokens        int64      `json:"tokens"`
	Model         string     `json:"model,omitempty"`
	PromptSHA256  string     `json:"prompt_sha256,omitempty"`
	SkillsUsed    string     `json:"skills_used,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	RedactedAt    *time.Time `json:"redacted_at,omitempty"`
}

// ListRunsForEvidence returns runs started in a window, oldest first so the
// section reads as a timeline and two packs over the same window are identical.
func ListRunsForEvidence(ctx context.Context, from, to time.Time) ([]*EvidenceRun, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT id, agent_id, trigger_source, status, step_count, tokens,
		model, prompt_sha256, skills_used, started_at, ended_at, redacted_at
		FROM ai_agent_runs
		WHERE started_at >= $1 AND started_at < $2
		ORDER BY started_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, from, to)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRunsForEvidence err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := []*EvidenceRun{}
	for rows.Next() {
		var r EvidenceRun
		var modelName, promptSHA, skills sql.NullString
		var endedAt, redactedAt sql.NullTime
		if scanErr := rows.Scan(&r.Id, &r.AgentId, &r.TriggerSource, &r.Status, &r.StepCount,
			&r.Tokens, &modelName, &promptSHA, &skills, &r.StartedAt, &endedAt, &redactedAt); scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListRunsForEvidence scan err: %+v", scanErr)
			return nil, scanErr
		}
		r.Model = modelName.String
		r.PromptSHA256 = promptSHA.String
		if skills.Valid && skills.String != "[]" {
			r.SkillsUsed = skills.String
		}
		if endedAt.Valid {
			r.EndedAt = &endedAt.Time
		}
		if redactedAt.Valid {
			r.RedactedAt = &redactedAt.Time
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func ListRunsByAgent(ctx context.Context, agentId uuid.UUID, limit int) ([]*AgentRun, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 100 {
		limit = 25
	}
	const q = `SELECT id, agent_id, trigger_source, run_as_user_id, status, steps,
		step_count, tokens, result, error, started_at, ended_at, redacted_at,
		model, prompt_sha256, skills_used
		FROM ai_agent_runs WHERE agent_id=$1 ORDER BY started_at DESC LIMIT $2`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, agentId, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRunsByAgent Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AgentRun
	for rows.Next() {
		var r AgentRun
		var runAs uuid.NullUUID
		var result, errMsg sql.NullString
		var endedAt, redactedAt sql.NullTime
		var modelName, promptSHA, skillsUsed sql.NullString
		if scanErr := rows.Scan(&r.Id, &r.AgentId, &r.TriggerSource, &runAs, &r.Status,
			&r.Steps, &r.StepCount, &r.Tokens, &result, &errMsg, &r.StartedAt, &endedAt, &redactedAt,
			&modelName, &promptSHA, &skillsUsed); scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListRunsByAgent scan err: %+v", scanErr)
			return nil, scanErr
		}
		if runAs.Valid {
			r.RunAsUserId = &runAs.UUID
		}
		if result.Valid {
			r.Result = &result.String
		}
		if errMsg.Valid {
			r.Error = &errMsg.String
		}
		if endedAt.Valid {
			r.EndedAt = &endedAt.Time
		}
		if redactedAt.Valid {
			r.RedactedAt = &redactedAt.Time
		}
		r.Model = modelName.String
		r.PromptSHA256 = promptSHA.String
		// "[]" carries no information the client needs; omitempty then keeps it
		// off the wire for the runs that used no skills, which is most of them.
		if skillsUsed.Valid && skillsUsed.String != "[]" {
			r.SkillsUsed = skillsUsed.String
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// AgentRunActivity is one run enriched with its agent's display fields, for the
// cross-agent "what my agents did" activity feed. It reuses the ai_agent_runs
// record (the authoritative per-run transcript + outcome) joined to ai_agents,
// so there is no parallel activity store.
type AgentRunActivity struct {
	AgentRun
	AgentName      string  `json:"agent_name"`
	AgentAvatarKey *string `json:"agent_avatar_key,omitempty"`
}

// ExecRecentRuns runs a pre-built recent-runs activity query (see domain/AIAgent,
// which owns the optional created_by scoping + ordering/limit) and scans the
// joined run+agent rows newest-first into render-ready activity items.
func ExecRecentRuns(ctx context.Context, query string, args []interface{}) ([]*AgentRunActivity, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecRecentRuns failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AgentRunActivity
	for rows.Next() {
		var it AgentRunActivity
		var runAs uuid.NullUUID
		var result, errMsg, avatarKey sql.NullString
		var endedAt sql.NullTime
		if scanErr := rows.Scan(&it.Id, &it.AgentId, &it.TriggerSource, &runAs, &it.Status,
			&it.Steps, &it.StepCount, &it.Tokens, &result, &errMsg, &it.StartedAt, &endedAt,
			&it.AgentName, &avatarKey); scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ExecRecentRuns scan err: %+v", scanErr)
			return nil, scanErr
		}
		if runAs.Valid {
			it.RunAsUserId = &runAs.UUID
		}
		if result.Valid {
			it.Result = &result.String
		}
		if errMsg.Valid {
			it.Error = &errMsg.String
		}
		if endedAt.Valid {
			it.EndedAt = &endedAt.Time
		}
		if avatarKey.Valid {
			it.AgentAvatarKey = &avatarKey.String
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// RedactRunsOlderThan clears the transcript and free text of agent runs older
// than cutoff, returning how many were affected.
//
// Clears content, keeps the row, for a different reason than the audit log's
// redaction. This table is not hash-chained, so deleting would break no
// integrity guarantee. It would break the NUMBERS: the acceptance record, the
// activity feed and token accounting are computed over these rows, and removing
// them would quietly rewrite an agent's history.
//
// What survives is what those metrics need and what a person can act on:
// status, step count, tokens, and timings. What goes is the transcript and the
// result and error text, which is where workspace content lives.
//
// Idempotent: already-redacted rows are skipped.
func RedactRunsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_agent_runs
	              SET steps       = '[]'::jsonb,
	                  result      = NULL,
	                  error       = NULL,
	                  redacted_at = NOW()
	            WHERE started_at < $1 AND redacted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, cutoff)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RedactRunsOlderThan err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AnyAgentExists reports whether the workspace has at least one agent.
func AnyAgentExists(ctx context.Context) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var ok bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT EXISTS (SELECT 1 FROM ai_agents WHERE deleted_at IS NULL)`).Scan(&ok)
	return ok, err
}
