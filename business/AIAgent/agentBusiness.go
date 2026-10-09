// Package business (AIAgent) is the management layer for the Agent Builder:
// validation, ownership-scoped CRUD, and run history. The runner (the LLM
// tool-loop that executes an agent) lives separately; this package only governs
// agent definitions. Mirrors the Workflow Builder's Actor/ownership model so an
// agent can only be managed by its creator (or an admin).
package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// Limits keep an agent definition sane and bound runaway configs.
const (
	maxNameLen          = 120
	maxDescriptionLen   = 500
	maxInstructionsLen  = 8000
	maxToolsPerAgent    = 40
	defaultMaxSteps     = 8
	maxKnowledgeSources = 20
	maxSkillsPerAgent   = 10
	maxSkillNameLen     = 120
	maxSkillBodyLen     = 8000
	// maxAgentDailyTokensCeiling clamps a per-agent daily token cap so a typo
	// can't set an absurd value. Generous: real cost ceilings are the workspace
	// cap + the per-run token cap.
	maxAgentDailyTokensCeiling = 100_000_000
)

// KnowledgeRef is a curated grounding source attached to an agent: a channel,
// doc, or project the agent should always read for context. Stored on the agent
// and pulled at run time AS THE OWNER (permissions re-checked, inaccessible
// sources skipped).
type KnowledgeRef struct {
	Type  string `json:"type"` // channel | doc | project
	Id    string `json:"id"`
	Label string `json:"label,omitempty"`
}

func validKnowledgeType(t string) bool {
	switch t {
	case "channel", "doc", "project":
		return true
	default:
		return false
	}
}

// Actor is the user performing an agent management action.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
	// DgraphUID is the caller's graph identity, needed to ask the surface's OWN
	// visibility query whether this person can see a channel or a task at all
	// (see agentWorkEntity.go). Optional: a blank value simply means no
	// surface-level visibility can be proven, so only the people already
	// involved in a job can see it.
	DgraphUID string
}

// AgentInput is the create/update payload from the controller.
type AgentInput struct {
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	AvatarKey      string                 `json:"avatar_key"`
	Instructions   string                 `json:"instructions"`
	ModelPref      string                 `json:"model_pref"`
	EnabledTools   []string               `json:"enabled_tools"`
	TriggerType    string                 `json:"trigger_type"`
	TriggerConfig  map[string]interface{} `json:"trigger_config"`
	Scope          map[string]interface{} `json:"scope"`
	MaxSteps       int                    `json:"max_steps"`
	IsActive       bool                   `json:"is_active"`
	DmAble         bool                   `json:"dm_able"`
	Autonomy       string                 `json:"autonomy"`
	Knowledge      []KnowledgeRef         `json:"knowledge"`
	SkillIds       []string               `json:"skill_ids"`
	MaxDailyTokens int                    `json:"max_daily_tokens"`
	// RunInBackground opts the agent into durable, progress-reporting async
	// runs for channel/DM mentions (default false = today's instant reply).
	RunInBackground bool `json:"run_in_background"`
	// Ambient opts the agent into replying in its scoped channels without an
	// @mention (default false). AmbientKeywords narrows which messages it
	// considers (comma/newline separated; empty = only questions).
	Ambient         bool   `json:"ambient"`
	AmbientKeywords string `json:"ambient_keywords"`
	// A remote brain (AG-UI). AGUIEndpoint empty means the agent runs on this
	// workspace's model and the other two are ignored. AGUIAuthSecret is
	// write-only: empty on update keeps the stored secret, so a client that
	// never sees it cannot accidentally erase it; clearing the endpoint clears
	// the secret with it.
	AGUIEndpoint   string `json:"agui_endpoint"`
	AGUIAuthHeader string `json:"agui_auth_header"`
	AGUIAuthSecret string `json:"agui_auth_secret"`
	// RemoteProtocol is how the remote brain is reached: "agui" (default) or
	// "a2a". For A2A, AGUIEndpoint is the agent's address or its card's.
	RemoteProtocol string `json:"remote_protocol"`
}

var (
	errForbidden = fmt.Errorf("not authorized to manage this agent")
	errNotFound  = fmt.Errorf("agent not found")
)

// IsForbidden / IsNotFound let the controller map business errors to HTTP codes.
func IsForbidden(err error) bool { return err == errForbidden }
func IsNotFound(err error) bool  { return err == errNotFound }

// canManage delegates to the model so the API-token binding path, which cannot import
// this package, asks the identical question rather than carrying a second copy.
func canManage(actor Actor, a *model.AiAgent) bool {
	return a.ManageableBy(actor.UserID, actor.IsAdmin)
}

// validated holds the cleaned, JSON-encoded form ready to persist.
type validated struct {
	name             string
	description      *string
	avatarKey        *string
	instructions     string
	modelPref        *string
	enabledToolsJSON string
	triggerType      string
	triggerCfgJSON   string
	scopeJSON        string
	maxSteps         int
	knowledgeJSON    string
	skillIdsJSON     string
	maxDailyTokens   int
	aguiEndpoint     string
	aguiAuthHeader   string
	remoteProtocol   string
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func validate(in *AgentInput) (*validated, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("name is too long")
	}
	if len(in.Description) > maxDescriptionLen {
		return nil, fmt.Errorf("description is too long")
	}
	if len(in.Instructions) > maxInstructionsLen {
		return nil, fmt.Errorf("instructions are too long")
	}

	triggerType := strings.TrimSpace(in.TriggerType)
	if triggerType == "" {
		triggerType = model.TriggerManual
	}
	if !model.ValidTriggerType(triggerType) {
		return nil, fmt.Errorf("invalid trigger type")
	}

	if len(in.EnabledTools) > maxToolsPerAgent {
		return nil, fmt.Errorf("too many tools enabled")
	}
	// De-dupe + drop blanks; the runner separately enforces that each tool is
	// real and permitted at execution time.
	seen := map[string]bool{}
	tools := make([]string, 0, len(in.EnabledTools))
	for _, t := range in.EnabledTools {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tools = append(tools, t)
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("invalid tools")
	}

	cfg := in.TriggerConfig
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid trigger config")
	}

	scope := in.Scope
	if scope == nil {
		scope = map[string]interface{}{}
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return nil, fmt.Errorf("invalid scope")
	}

	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	if maxSteps > model.MaxStepsCeiling {
		maxSteps = model.MaxStepsCeiling
	}

	// Knowledge sources: de-dupe + drop blanks/invalid; cap. Read-time access is
	// re-checked AS THE OWNER, so validation here is just shape + bound.
	knownSeen := map[string]bool{}
	knowledge := make([]KnowledgeRef, 0, len(in.Knowledge))
	for _, k := range in.Knowledge {
		k.Type = strings.TrimSpace(k.Type)
		k.Id = strings.TrimSpace(k.Id)
		k.Label = strings.TrimSpace(k.Label)
		if k.Id == "" || !validKnowledgeType(k.Type) {
			continue
		}
		key := k.Type + ":" + k.Id
		if knownSeen[key] {
			continue
		}
		knownSeen[key] = true
		knowledge = append(knowledge, k)
		if len(knowledge) >= maxKnowledgeSources {
			break
		}
	}
	knowledgeJSON, err := json.Marshal(knowledge)
	if err != nil {
		return nil, fmt.Errorf("invalid knowledge sources")
	}

	// Skill ids: keep valid, de-duped uuids; cap. Existence is checked lazily at
	// run time (a deleted skill is simply omitted), so we only validate shape.
	skillSeen := map[string]bool{}
	skillIds := make([]string, 0, len(in.SkillIds))
	for _, raw := range in.SkillIds {
		s := strings.TrimSpace(raw)
		if s == "" || skillSeen[s] {
			continue
		}
		if _, perr := uuid.Parse(s); perr != nil {
			continue
		}
		skillSeen[s] = true
		skillIds = append(skillIds, s)
		if len(skillIds) >= maxSkillsPerAgent {
			break
		}
	}
	skillIdsJSON, err := json.Marshal(skillIds)
	if err != nil {
		return nil, fmt.Errorf("invalid skills")
	}

	// Per-agent daily token cap: a non-negative bound (0 = no cap, only the
	// workspace cap applies). Clamped to a sane ceiling so a typo can't set an
	// absurd value; the real backstops are the workspace cap + per-run cap.
	maxDailyTokens := in.MaxDailyTokens
	if maxDailyTokens < 0 {
		maxDailyTokens = 0
	}
	if maxDailyTokens > maxAgentDailyTokensCeiling {
		maxDailyTokens = maxAgentDailyTokensCeiling
	}

	aguiEndpoint, aguiAuthHeader, err := validateRemoteBrain(in.AGUIEndpoint, in.AGUIAuthHeader, in.AGUIAuthSecret)
	if err != nil {
		return nil, err
	}
	remoteProtocol, err := validateRemoteProtocol(in.RemoteProtocol)
	if err != nil {
		return nil, err
	}

	return &validated{
		name:             name,
		description:      optStr(in.Description),
		avatarKey:        optStr(in.AvatarKey),
		instructions:     in.Instructions,
		modelPref:        optStr(in.ModelPref),
		enabledToolsJSON: string(toolsJSON),
		triggerType:      triggerType,
		triggerCfgJSON:   string(cfgJSON),
		scopeJSON:        string(scopeJSON),
		maxSteps:         maxSteps,
		knowledgeJSON:    string(knowledgeJSON),
		skillIdsJSON:     string(skillIdsJSON),
		maxDailyTokens:   maxDailyTokens,
		aguiEndpoint:     aguiEndpoint,
		aguiAuthHeader:   aguiAuthHeader,
		remoteProtocol:   remoteProtocol,
	}, nil
}

// validateAgentModelPref ensures an agent's pinned model (when set) is a usable
// entry in the admin authorized-models allowlist, so an agent can never be
// bound to a model the admin has not authorized (or whose provider is
// disabled). An empty pref means "use the workspace default" and is always ok.
// The stored value is the authorized_models row id (string), mirroring the
// per-user model preference.
func validateAgentModelPref(ctx context.Context, modelPref *string) error {
	if modelPref == nil || strings.TrimSpace(*modelPref) == "" {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*modelPref))
	if err != nil {
		return fmt.Errorf("invalid model selection")
	}
	m, err := aiModels.GetAuthorizedModel(ctx, id)
	if err != nil {
		return fmt.Errorf("selected model is not available")
	}
	if !m.Usable() {
		return fmt.Errorf("model %q is not available (it or its provider is disabled)", m.DisplayLabel())
	}
	return nil
}

// CreateAgent validates and persists a new agent owned by createdBy.
func CreateAgent(ctx context.Context, in AgentInput, createdBy uuid.UUID) (*model.AiAgent, error) {
	if err := normalizeAgentMoveFilter(ctx, &in); err != nil {
		return nil, err
	}
	v, err := validate(&in)
	if err != nil {
		return nil, err
	}
	if err := validateAgentModelPref(ctx, v.modelPref); err != nil {
		return nil, err
	}
	if err := reach.checkReach(ctx, createdBy.String(), v.triggerType, v.triggerCfgJSON, v.scopeJSON); err != nil {
		return nil, err
	}
	a := &model.AiAgent{
		Name:            v.name,
		Description:     v.description,
		AvatarKey:       v.avatarKey,
		Instructions:    v.instructions,
		ModelPref:       v.modelPref,
		EnabledTools:    v.enabledToolsJSON,
		TriggerType:     v.triggerType,
		TriggerConfig:   v.triggerCfgJSON,
		Scope:           v.scopeJSON,
		MaxSteps:        v.maxSteps,
		IsActive:        in.IsActive,
		DmAble:          in.DmAble,
		Autonomy:        in.Autonomy,
		Knowledge:       v.knowledgeJSON,
		SkillIds:        v.skillIdsJSON,
		MaxDailyTokens:  v.maxDailyTokens,
		RunInBackground: in.RunInBackground,
		Ambient:         in.Ambient,
		AmbientKeywords: strings.TrimSpace(in.AmbientKeywords),
		AGUIEndpoint:    v.aguiEndpoint,
		AGUIAuthHeader:  v.aguiAuthHeader,
		AGUIAuthSecret:  remoteSecretToStore(v.aguiEndpoint, in.AGUIAuthSecret, ""),
		RemoteProtocol:  v.remoteProtocol,
		CreatedBy:       createdBy,
	}
	if a.RemoteCard, err = resolveRemoteCard(ctx, v.remoteProtocol, a.AGUIEndpoint, a.AGUIAuthHeader, a.AGUIAuthSecret); err != nil {
		return nil, err
	}
	id, err := model.CreateAgent(ctx, a)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateAgent failed err: %+v", err)
		return nil, fmt.Errorf("failed to create agent")
	}
	go ReloadTriggerCache(context.WithoutCancel(ctx))
	return model.GetAgentByID(ctx, id)
}

// UpdateAgent validates and persists changes to an agent the actor may manage.
func UpdateAgent(ctx context.Context, id uuid.UUID, in AgentInput, actor Actor) (*model.AiAgent, error) {
	existing, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if existing == nil {
		return nil, errNotFound
	}
	if !canManage(actor, existing) {
		return nil, errForbidden
	}
	if err := normalizeAgentMoveFilter(ctx, &in); err != nil {
		return nil, err
	}
	v, err := validate(&in)
	if err != nil {
		return nil, err
	}
	if err := validateAgentModelPref(ctx, v.modelPref); err != nil {
		return nil, err
	}
	// It acts as the person it works for, whoever edits it.
	kept := ""
	if strings.TrimSpace(existing.TriggerType) == model.TriggerEvent {
		kept = parseTriggerConfig(existing).Event
	}
	if err := reach.checkReachKeeping(ctx, existing.CreatedBy.String(), kept, v.triggerType, v.triggerCfgJSON, v.scopeJSON); err != nil {
		return nil, err
	}
	existing.Name = v.name
	existing.Description = v.description
	existing.AvatarKey = v.avatarKey
	existing.Instructions = v.instructions
	existing.ModelPref = v.modelPref
	existing.EnabledTools = v.enabledToolsJSON
	existing.TriggerType = v.triggerType
	existing.TriggerConfig = v.triggerCfgJSON
	existing.Scope = v.scopeJSON
	existing.MaxSteps = v.maxSteps
	existing.IsActive = in.IsActive
	existing.DmAble = in.DmAble
	existing.Autonomy = in.Autonomy
	existing.Knowledge = v.knowledgeJSON
	existing.SkillIds = v.skillIdsJSON
	existing.MaxDailyTokens = v.maxDailyTokens
	existing.RunInBackground = in.RunInBackground
	existing.Ambient = in.Ambient
	existing.AmbientKeywords = strings.TrimSpace(in.AmbientKeywords)
	existing.AGUIAuthSecret = remoteSecretToStore(v.aguiEndpoint, in.AGUIAuthSecret, existing.AGUIAuthSecret)
	existing.AGUIEndpoint = v.aguiEndpoint
	existing.AGUIAuthHeader = v.aguiAuthHeader
	existing.RemoteProtocol = v.remoteProtocol
	if existing.RemoteCard, err = resolveRemoteCard(ctx, v.remoteProtocol, existing.AGUIEndpoint, existing.AGUIAuthHeader, existing.AGUIAuthSecret); err != nil {
		return nil, err
	}
	if err := model.UpdateAgent(ctx, existing); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateAgent failed err: %+v", err)
		return nil, fmt.Errorf("failed to update agent")
	}
	go ReloadTriggerCache(context.WithoutCancel(ctx))
	return model.GetAgentByID(ctx, id)
}

// SetActive toggles an agent the actor may manage.
func SetActive(ctx context.Context, id uuid.UUID, isActive bool, actor Actor) error {
	existing, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to load agent")
	}
	if existing == nil {
		return errNotFound
	}
	if !canManage(actor, existing) {
		return errForbidden
	}
	if err := model.SetAgentActive(ctx, id, isActive); err != nil {
		return err
	}
	if !isActive {
		// Paused means stopped: its queued and running jobs too.
		stopAgentWork(ctx, id, actor.UserID)
	}
	go ReloadTriggerCache(context.WithoutCancel(ctx))
	return nil
}

// DeleteAgent soft-deletes an agent the actor may manage.
func DeleteAgent(ctx context.Context, id uuid.UUID, actor Actor) error {
	existing, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to load agent")
	}
	if existing == nil {
		return errNotFound
	}
	if !canManage(actor, existing) {
		return errForbidden
	}
	if err := model.SoftDeleteAgent(ctx, id); err != nil {
		return err
	}
	stopAgentWork(ctx, id, actor.UserID)
	// Drop the cached per-agent bot principal so a recreated/renamed agent does
	// not serve a stale identity (the bot user row is left intact since it owns
	// the agent's previously authored messages).
	userBusiness.InvalidateAgentBot(id)
	go ReloadTriggerCache(context.WithoutCancel(ctx))
	return nil
}

// ListAgents returns agents visible to the actor: admins see all, members see
// only the ones they created.
//
// Each agent is labelled with the person who authorised it. That is the whole
// claim this product makes about agents — one cannot do what the person behind
// it could not — and the list named the tools, the autonomy and the budget
// while never naming the person. An admin reading it could see what an agent
// may call and not whose permissions bound it.
func ListAgents(ctx context.Context, actor Actor) ([]*model.AiAgent, error) {
	var (
		agents []*model.AiAgent
		err    error
	)
	if actor.IsAdmin {
		agents, err = model.ListAgents(ctx)
	} else {
		agents, err = model.ListAgentsByCreator(ctx, actor.UserID)
	}
	if err != nil {
		return nil, err
	}
	labelOwners(ctx, agents)
	return agents, nil
}

// labelOwners fills in who authorised each agent, in one query for the whole
// list rather than one per row.
//
// Best effort: a list that renders without the labels is worth more than an
// error page, and the id is still on every row for anything that needs to be
// exact. A name that cannot be resolved is left empty rather than guessed, so
// the interface can say so in its own words.
func labelOwners(ctx context.Context, agents []*model.AiAgent) {
	if len(agents) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.CreatedBy)
	}
	names, err := userModel.DisplayNamesByUUIDs(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ListAgents could not label owners err: %+v", err)
		return
	}
	labelOwnersFrom(agents, names)
}

// labelOwnersFrom applies resolved names. Split from the lookup so the rule —
// every agent labelled, an unresolved one left empty, the id untouched — can be
// tested without a database, which is where it would otherwise never be tested
// at all.
func labelOwnersFrom(agents []*model.AiAgent, names map[uuid.UUID]string) {
	for _, a := range agents {
		a.CreatedByName = names[a.CreatedBy]
	}
}

// GetAgent returns a single agent the actor may manage.
func GetAgent(ctx context.Context, id uuid.UUID, actor Actor) (*model.AiAgent, error) {
	a, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if a == nil {
		return nil, errNotFound
	}
	if !canManage(actor, a) {
		return nil, errForbidden
	}
	return a, nil
}

// ListRuns returns recent run history for an agent the actor may manage.
func ListRuns(ctx context.Context, id uuid.UUID, actor Actor, limit int) ([]*model.AgentRun, error) {
	a, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if a == nil {
		return nil, errNotFound
	}
	if !canManage(actor, a) {
		return nil, errForbidden
	}
	return model.ListRunsByAgent(ctx, id, limit)
}

// WorkspaceAgentStats returns the fleet-level rollup the actor may see: admins
// get the whole workspace, members only the agents they created. Read-only.
func WorkspaceAgentStats(ctx context.Context, actor Actor) (*model.WorkspaceAgentStats, error) {
	var createdBy *uuid.UUID
	if !actor.IsAdmin {
		id := actor.UserID
		createdBy = &id
	}
	return agentDomain.GetWorkspaceAgentStats(ctx, createdBy)
}

// AgentHealthBatch returns the compact per-agent health signal for every agent
// the actor may see (admins: whole workspace; members: their own), keyed by
// agent id, in a single query. Powers the at-a-glance status dot per row in the
// agents list. Read-only; same actor scoping as WorkspaceAgentStats.
func AgentHealthBatch(ctx context.Context, actor Actor) (map[string]*model.AgentHealth, error) {
	var createdBy *uuid.UUID
	if !actor.IsAdmin {
		id := actor.UserID
		createdBy = &id
	}
	return agentDomain.GetAgentHealthBatch(ctx, createdBy)
}

// AgentStats returns the reliability/activity rollup for an agent the actor may
// manage. Read-only; reuses the same ownership gate as the rest of the builder.
func AgentStats(ctx context.Context, id uuid.UUID, actor Actor) (*model.AgentRunStats, error) {
	a, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if a == nil {
		return nil, errNotFound
	}
	if !canManage(actor, a) {
		return nil, errForbidden
	}
	stats, err := model.GetAgentRunStats(ctx, id)
	if err != nil {
		return nil, err
	}
	// Enrich with the per-agent daily budget: today's spend + the configured
	// cap, so the reliability panel shows "AI today: used / cap".
	stats.MaxDailyTokens = a.MaxDailyTokens
	stats.TokensToday = ai.AgentTokenUsageToday(ctx, id.String())
	// Enrich with the per-agent sandbox budget the same way — but only when the
	// execution sandbox is enabled, so the panel shows "Sandbox today: runs/cap"
	// exclusively where the capability actually applies (no noise otherwise).
	if ai.SandboxEnabled() {
		stats.SandboxDailyRuns = a.SandboxDailyRuns
		stats.SandboxDailySeconds = a.SandboxDailySeconds
		if u, uerr := aiModels.AgentSandboxUsageToday(ctx, id); uerr == nil {
			stats.SandboxRunsToday = u.Runs
			stats.SandboxSecondsToday = u.Seconds
		}
	}
	if notes, nerr := model.GetAgentState(ctx, id); nerr == nil {
		stats.WorkingNotes = notes
	}
	return stats, nil
}

// RunAgentManual executes an agent on demand (the builder's "test" action),
// for an actor who may manage it. The run executes as the agent's owner with
// per-call permission re-checks, and is for the actor: an admin testing someone
// else's agent reaches only what both of them can, not the owner's DMs or mail.
// dryRun previews without performing writes.
func RunAgentManual(ctx context.Context, id uuid.UUID, actor Actor, prompt string, dryRun bool) (*RunOutcome, error) {
	a, err := model.GetAgentByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load agent")
	}
	if a == nil {
		return nil, errNotFound
	}
	if !canManage(actor, a) {
		return nil, errForbidden
	}
	return RunAgent(WithAgentAskerWords(askedBy(ctx, actor.UserID.String()), prompt), a, "manual", prompt, dryRun), nil
}
