// Package business (ApiToken) mints, validates, and manages scoped API tokens
// for the public API. A token authenticates as its creating user; requests run
// with that user's permissions, narrowed to the token's scopes. Only a SHA-256
// hash is persisted — the plaintext is returned once at creation.
package business

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	model "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// Scope constants. Each maps to a class of operations on the /v1 surface; the
// route handlers assert the scope they need. Keep aligned with AllScopes (shown
// in the management UI) and the SDK.
const (
	ScopeTasksRead     = "tasks:read"
	ScopeTasksWrite    = "tasks:write"
	ScopeProjectsRead  = "projects:read"
	ScopeProjectsWrite = "projects:write"
	ScopeDocsRead      = "docs:read"
	ScopeDocsWrite     = "docs:write"
	ScopeMessagesRead  = "messages:read"
	ScopeMessagesWrite = "messages:write"
	ScopeCalendarWrite = "calendar:write"
	ScopeTablesRead    = "tables:read"
	ScopeTablesWrite   = "tables:write"
	ScopeSearchRead    = "search:read"
	// ScopeDataSourcesRead is distinct from tables:read on purpose: reaching an
	// external, connected database is a materially broader grant than reading
	// native tables, so a token must opt into it explicitly.
	ScopeDataSourcesRead = "data_sources:read"
	// ScopeAttentionRead reads what is waiting for the token's owner (unread
	// counts, approvals, overdue tasks) and nothing else: what a desktop bar
	// needs, without the reach of messages:read (GET /v1/unread, /v1/attention).
	ScopeAttentionRead = "attention:read"
)

// AllScopes is the catalog of grantable scopes (validation + UI).
var AllScopes = []string{
	ScopeTasksRead, ScopeTasksWrite,
	ScopeProjectsRead, ScopeProjectsWrite,
	ScopeDocsRead, ScopeDocsWrite,
	ScopeMessagesRead, ScopeMessagesWrite,
	ScopeCalendarWrite,
	ScopeTablesRead, ScopeTablesWrite,
	ScopeSearchRead,
	ScopeDataSourcesRead,
	ScopeAttentionRead,
}

// ToolScope maps each public AI tool (exposed over the MCP server endpoint and
// reused by the REST /v1 surface) to the scope a token must hold to invoke it.
// Tools not present here are NOT exposed publicly (e.g. per-user external
// connectors that require interactive OAuth + confirmation). Keep aligned with
// services/AI.ToolRegistry.
var ToolScope = map[string]string{
	// Tasks
	"list_tasks":         ScopeTasksRead,
	"list_project_tasks": ScopeTasksRead,
	"create_task":        ScopeTasksWrite,
	"update_task_status": ScopeTasksWrite,
	"assign_task":        ScopeTasksWrite,
	"set_task_due_date":  ScopeTasksWrite,
	// Projects / teams
	"list_projects":  ScopeProjectsRead,
	"read_project":   ScopeProjectsRead,
	"list_teams":     ScopeProjectsRead,
	"create_project": ScopeProjectsWrite,
	// Docs
	"read_doc":   ScopeDocsRead,
	"create_doc": ScopeDocsWrite,
	// Messages
	"summarize_channel":    ScopeMessagesRead,
	"summarize_dm":         ScopeMessagesRead,
	"summarize_group_chat": ScopeMessagesRead,
	"send_message":         ScopeMessagesWrite,
	"send_dm":              ScopeMessagesWrite,
	"send_group_chat":      ScopeMessagesWrite,
	// Calendar
	"set_reminder": ScopeCalendarWrite,
	// Tables
	"list_tables":      ScopeTablesRead,
	"read_table":       ScopeTablesRead,
	"query_table":      ScopeTablesRead,
	"query_plan":       ScopeTablesRead,
	"create_table_row": ScopeTablesWrite,
	"update_table_row": ScopeTablesWrite,
	// External data sources (read-only). Distinct scope from tables.
	"list_data_sources":      ScopeDataSourcesRead,
	"read_data_source":       ScopeDataSourcesRead,
	"query_data_source":      ScopeDataSourcesRead,
	"query_data_source_plan": ScopeDataSourcesRead,
	// Search (cross-source recall: workspace + Memory + connected apps)
	"search_workspace": ScopeSearchRead,
}

// ScopeForTool returns the scope required to call a tool and whether the tool
// is publicly exposable at all.
func ScopeForTool(tool string) (string, bool) {
	s, ok := ToolScope[tool]
	return s, ok
}

const (
	tokenPlainPrefix = "oc_" // human-recognizable token prefix
	maxNameLen       = 120
	maxTokensPerUser = 50
)

var errNotFound = fmt.Errorf("token not found")

// CreatedToken is returned once at creation: the row plus the one-time plaintext.
type CreatedToken struct {
	Token     *model.ApiToken `json:"token"`
	Plaintext string          `json:"plaintext"` // shown once; never retrievable again
}

// validScope reports whether s is a known grantable scope.
func validScope(s string) bool {
	for _, sc := range AllScopes {
		if sc == s {
			return true
		}
	}
	return false
}

// hashToken returns the SHA-256 hex of a token's plaintext.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// generateSecret returns a new token plaintext ("oc_" + 40 hex chars).
func generateSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenPlainPrefix + hex.EncodeToString(buf), nil
}

// CreateToken validates scopes, mints a token, persists its hash, and returns
// the plaintext once. expiresInDays <= 0 means no expiry.
//
// agentID optionally binds the credential to an AGENT IDENTITY. Until this existed,
// api_tokens.agent_id could be read but never written, so the whole agent-identity
// mechanism was dormant: every credential resolved to an api_client, the per-agent
// kill switch had nothing to stop, and the per-agent token cap had nothing to meter.
//
// BINDING IS AN ADMINISTRATIVE ACT ON THE AGENT, not on the token, so it asks the
// agent's own management rule (owner or admin) via the shared model method. A member
// cannot lend their authority to someone else's agent, which is what stops an agent
// identity quietly becoming the shared account everyone hides inside.
//
// The credential still carries its OWNER's authority — binding changes attribution,
// budget and the kill switch, never permissions. The audit row records both the
// principal and the agent so a reviewer can always see whose authority was used.
func CreateToken(ctx context.Context, name string, scopes []string, expiresInDays int, createdBy uuid.UUID, agentID *uuid.UUID, isAdmin bool) (*CreatedToken, error) {
	var expiresAt *time.Time
	if expiresInDays > 0 {
		t := time.Now().AddDate(0, 0, expiresInDays)
		expiresAt = &t
	}
	return MintToken(ctx, name, scopes, expiresAt, createdBy, agentID, isAdmin)
}

// MintToken is CreateToken with an exact expiry, for a credential that lives
// for an hour rather than for days: the access token an OAuth-connected client
// holds. Same checks, same row, so it is listed, revoked and killed like any
// other credential.
func MintToken(ctx context.Context, name string, scopes []string, expiresAt *time.Time, createdBy uuid.UUID, agentID *uuid.UUID, isAdmin bool) (*CreatedToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("name is too long")
	}

	// Resolve the agent binding BEFORE minting anything, so a refusal never leaves a
	// usable credential behind.
	if agentID != nil {
		agent, aerr := agentModel.GetAgentByID(ctx, *agentID)
		if aerr != nil || agent == nil {
			return nil, fmt.Errorf("agent not found")
		}
		if !agent.ManageableBy(createdBy, isAdmin) {
			// Same text as "not found" would be safer against enumeration, but an agent
			// list is already readable by any member, so there is nothing to conceal and
			// a clear reason is worth more than a pointless ambiguity.
			return nil, fmt.Errorf("you can only bind a token to an agent you own")
		}
		if !agent.IsActive {
			// Refusing rather than allowing a credential that would be rejected on every
			// call: the failure would surface later, in a client, as an unexplained
			// permission error.
			return nil, fmt.Errorf("agent %q is deactivated; reactivate it before binding a token", agent.Name)
		}
	}

	// De-dupe + validate scopes.
	seen := map[string]bool{}
	clean := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !validScope(s) {
			return nil, fmt.Errorf("unknown scope: %s", s)
		}
		seen[s] = true
		clean = append(clean, s)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("select at least one scope")
	}

	existing, err := model.ListByUser(ctx, createdBy)
	if err == nil {
		active := 0
		for _, t := range existing {
			if t.RevokedAt == nil {
				active++
			}
		}
		if active >= maxTokensPerUser {
			return nil, fmt.Errorf("token limit reached; revoke an unused token first")
		}
	}

	plaintext, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("failed to generate token")
	}
	scopesJSON, _ := json.Marshal(clean)

	prefix := displayPrefix(plaintext)

	id, err := model.CreateToken(ctx, name, hashToken(plaintext), prefix, string(scopesJSON), createdBy, expiresAt, agentID)
	if err != nil {
		return nil, fmt.Errorf("failed to create token")
	}
	row := &model.ApiToken{
		Id: id, Name: name, TokenPrefix: prefix, Scopes: string(scopesJSON),
		CreatedBy: createdBy, ExpiresAt: expiresAt, AgentId: agentID,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	return &CreatedToken{Token: row, Plaintext: plaintext}, nil
}

// ListTokens returns a user's tokens (no secrets).
func ListTokens(ctx context.Context, createdBy uuid.UUID) ([]*model.ApiToken, error) {
	return model.ListByUser(ctx, createdBy)
}

// RevokeToken revokes a token the user owns.
func RevokeToken(ctx context.Context, id, createdBy uuid.UUID) error {
	if err := model.RevokeToken(ctx, id, createdBy); err != nil {
		return errNotFound
	}
	return nil
}

// AuthResult is what the middleware needs after validating a presented token.
type AuthResult struct {
	UserID  uuid.UUID
	TokenID uuid.UUID
	Scopes  []string
	// AgentID is the AGENT IDENTITY this credential authenticates, when it is bound
	// to one (migration 138). Nil for a plain integration credential.
	//
	// Deliberately just the id: whether that agent is still ACTIVE is a live
	// question and must be asked per call by whoever cares, not cached here. An
	// agent's is_active flag is an independent kill switch, and a kill switch that
	// takes effect at the next token rotation is not a kill switch.
	AgentID *uuid.UUID
}

// Validate authenticates a presented token plaintext: looks it up by hash,
// confirms it is active, records last-used (async), and returns the owner +
// granted scopes. Returns (nil, nil) when the token is missing/invalid so the
// middleware can answer 401 uniformly.
func Validate(ctx context.Context, plaintext string) (*AuthResult, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" || !strings.HasPrefix(plaintext, tokenPlainPrefix) {
		return nil, nil
	}
	row, err := model.GetActiveByHash(ctx, hashToken(plaintext))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	// Best-effort last-used touch, throttled to at most once/min/token so a
	// busy integration doesn't issue a DB UPDATE on every request. The window
	// gate fails open (Redis down → we still touch), and the touch itself is
	// async and non-blocking.
	if shouldTouchLastUsed(ctx, row.Id) {
		go model.TouchLastUsed(context.Background(), row.Id)
	}

	var scopes []string
	_ = json.Unmarshal([]byte(row.Scopes), &scopes)
	return &AuthResult{UserID: row.CreatedBy, TokenID: row.Id, Scopes: scopes, AgentID: row.AgentId}, nil
}

// shouldTouchLastUsed returns true at most once per token per minute (when
// Redis is reachable). Fails open: if Redis is unavailable it returns true so
// the timestamp still updates, accepting the per-request write only during an
// outage.
func shouldTouchLastUsed(ctx context.Context, tokenID uuid.UUID) bool {
	if !redisStore.IsAvailable() {
		return true
	}
	res := redisStore.AllowFixedWindow(ctx, registry.ApiTokenTouch, []string{tokenID.String()}, 1)
	return res.Allowed
}

// HasScope reports whether the granted scope set includes required.
func HasScope(granted []string, required string) bool {
	for _, s := range granted {
		if s == required {
			return true
		}
	}
	return false
}

// BoundAgent answers, for a credential bound to an agent, whether that agent may
// act right now: it returns the agent, or a reason it may not. Asked per request
// by every surface a credential reaches (MCP and the public REST API), because
// ai_agents.is_active is the kill switch and a switch one surface ignores is not
// one.
//
// Fails closed and never panics: the database accessors dereference a
// connection that is nil until the server has connected, and for an identity
// question the only safe failure is a refusal.
func BoundAgent(ctx context.Context, agentID uuid.UUID) (agent *agentModel.AiAgent, reason string) {
	const unverified = "the agent identity behind this credential could not be verified"
	defer func() {
		if r := recover(); r != nil {
			agent, reason = nil, unverified
		}
	}()
	a, err := agentModel.GetAgentByID(ctx, agentID)
	switch {
	case err != nil || a == nil:
		return nil, unverified
	case a.DeletedAt != nil:
		return nil, "the agent identity behind this credential has been deleted"
	case !a.IsActive:
		return nil, "the agent identity behind this credential is deactivated"
	}
	return a, ""
}

// displayPrefix is "oc_" plus the first 8 secret characters: enough to
// recognise a credential in a list without revealing it.
func displayPrefix(plaintext string) string {
	if len(plaintext) > 11 {
		return plaintext[:11]
	}
	return plaintext
}

// RotateSecret gives a live credential a new secret and a new expiry, keeping
// its id, name, scopes and agent. The old secret stops working at once. This is
// how an OAuth refresh renews access without a new row per hour: the inventory
// keeps showing one credential per connection, and revoking it ends the
// connection.
func RotateSecret(ctx context.Context, id uuid.UUID, expiresAt time.Time) (string, error) {
	plaintext, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate token")
	}
	if err := model.RotateSecret(ctx, id, hashToken(plaintext), displayPrefix(plaintext), expiresAt); err != nil {
		return "", err
	}
	return plaintext, nil
}

// ToolsForScopes lists the public tools a set of scopes reaches, sorted. An
// agent identity made for a connected client is given exactly these, so what
// the person ticked on the consent screen is what the agent can call.
func ToolsForScopes(scopes []string) []string {
	granted := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		granted[s] = true
	}
	var out []string
	for tool, scope := range ToolScope {
		if granted[scope] {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

// ValidScopes reports whether every scope is grantable. Exported for the OAuth
// server, which receives scopes from a client rather than from our own form.
func ValidScopes(scopes []string) bool {
	for _, s := range scopes {
		if !validScope(s) {
			return false
		}
	}
	return true
}

// HashToken is the stored form of a presented secret, for callers outside this
// package that look a credential up by what they were handed.
func HashToken(plaintext string) string { return hashToken(plaintext) }
