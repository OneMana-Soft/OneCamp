package business

// The agent inventory: everything that can act in this workspace without being
// a person at a keyboard, on one page, with who answers for it.
//
// Two kinds of thing can act. An AGENT is an identity with a sponsor, a reach
// and a brain. A CREDENTIAL (API token) is a key someone made for a script, an
// MCP client or an agent. An admin asking "what can act here" needs both,
// because a credential not bound to any agent is still a way in, and the
// agents list never showed it.
//
// Reads only what already exists: agents, the health rollup, the action log
// and the refusals the MCP gate writes to the audit log. Nothing here is a new
// source of truth.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// InventoryAgent is one agent as the inventory shows it.
type InventoryAgent struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	IsActive bool      `json:"is_active"`
	// Sponsor is the person whose permissions bound the agent. Empty when they
	// cannot be resolved; the client says so in its own words.
	SponsorID uuid.UUID `json:"sponsor_id"`
	Sponsor   string    `json:"sponsor"`
	// Brain is where the reasoning happens: "workspace", or "agui:host" /
	// "a2a:host" for a remote brain.
	Brain    string `json:"brain"`
	Autonomy string `json:"autonomy"`
	Trigger  string `json:"trigger"`
	// Reach: how many channels it is scoped to (0 = wherever it is mentioned)
	// and how many tools it may call.
	Channels int `json:"channels"`
	Tools    int `json:"tools"`
	// Credentials bound to this agent that can still be used.
	Credentials int `json:"credentials"`

	Runs7d        int64      `json:"runs_7d"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	Actions7d     int64      `json:"actions_7d"`
	Refusals7d    int64      `json:"refusals_7d"`
	LastRefusalAt *time.Time `json:"last_refusal_at,omitempty"`
}

// InventoryCredential is one live credential.
type InventoryCredential struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
	SponsorID   uuid.UUID  `json:"sponsor_id"`
	Sponsor     string     `json:"sponsor"`
	AgentID     *uuid.UUID `json:"agent_id,omitempty"`
	AgentName   string     `json:"agent_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Refusals7d  int64      `json:"refusals_7d"`
}

// Inventory is the whole page.
type Inventory struct {
	Agents      []InventoryAgent      `json:"agents"`
	Credentials []InventoryCredential `json:"credentials"`
	WindowDays  int                   `json:"window_days"`
}

// Seams for tests; the page is assembled without a database there.
var (
	inventoryAgentsFn   = ListAgents
	inventoryHealthFn   = AgentHealthBatch
	inventoryActivityFn = model.InventoryActivity
	inventoryTokensFn   = tokenModel.ListActive
	inventoryNamesFn    = userModel.DisplayNamesByUUIDs
)

// AgentInventory returns the inventory the actor may see: an admin sees the
// workspace, a member sees their own agents and credentials. The same rule as
// the agents list, so the inventory never shows a member something the list
// would not.
func AgentInventory(ctx context.Context, actor Actor) (*Inventory, error) {
	agents, err := inventoryAgentsFn(ctx, actor)
	if err != nil {
		return nil, err
	}
	health, err := inventoryHealthFn(ctx, actor)
	if err != nil {
		return nil, err
	}
	byAgent, byToken, err := inventoryActivityFn(ctx, time.Now().Add(-model.InventoryWindow))
	if err != nil {
		return nil, err
	}
	tokens, err := inventoryTokensFn(ctx)
	if err != nil {
		return nil, err
	}
	if !actor.IsAdmin {
		tokens = tokensMadeBy(tokens, actor.UserID)
	}
	names := sponsorNames(ctx, agents, tokens)
	return buildInventory(agents, health, byAgent, byToken, tokens, names), nil
}

func tokensMadeBy(tokens []*tokenModel.ApiToken, userID uuid.UUID) []*tokenModel.ApiToken {
	out := tokens[:0:0]
	for _, t := range tokens {
		if t.CreatedBy == userID {
			out = append(out, t)
		}
	}
	return out
}

// sponsorNames labels token makers. Agents already carry CreatedByName from
// ListAgents, so only people not seen there are looked up.
func sponsorNames(ctx context.Context, agents []*model.AiAgent, tokens []*tokenModel.ApiToken) map[uuid.UUID]string {
	names := map[uuid.UUID]string{}
	for _, a := range agents {
		if a.CreatedByName != "" {
			names[a.CreatedBy] = a.CreatedByName
		}
	}
	var missing []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, t := range tokens {
		if _, ok := names[t.CreatedBy]; !ok && !seen[t.CreatedBy] {
			seen[t.CreatedBy] = true
			missing = append(missing, t.CreatedBy)
		}
	}
	if len(missing) == 0 {
		return names
	}
	more, err := inventoryNamesFn(ctx, missing)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AgentInventory could not label sponsors err: %+v", err)
		return names
	}
	for id, n := range more {
		names[id] = n
	}
	return names
}

// buildInventory is the pure assembly, split out so it is tested whole.
func buildInventory(
	agents []*model.AiAgent,
	health map[string]*model.AgentHealth,
	byAgent, byToken map[string]*model.AgentActivity,
	tokens []*tokenModel.ApiToken,
	names map[uuid.UUID]string,
) *Inventory {
	inv := &Inventory{
		Agents:      make([]InventoryAgent, 0, len(agents)),
		Credentials: make([]InventoryCredential, 0, len(tokens)),
		WindowDays:  int(model.InventoryWindow / (24 * time.Hour)),
	}

	agentName := make(map[uuid.UUID]string, len(agents))
	boundCount := map[uuid.UUID]int{}
	for _, a := range agents {
		agentName[a.Id] = a.Name
	}
	for _, t := range tokens {
		c := InventoryCredential{
			ID:          t.Id,
			Name:        t.Name,
			TokenPrefix: t.TokenPrefix,
			Scopes:      nonNilStrings(parseTokenScopes(t.Scopes)),
			SponsorID:   t.CreatedBy,
			Sponsor:     names[t.CreatedBy],
			AgentID:     t.AgentId,
			CreatedAt:   t.CreatedAt,
			LastUsedAt:  t.LastUsedAt,
			ExpiresAt:   t.ExpiresAt,
		}
		if t.AgentId != nil {
			c.AgentName = agentName[*t.AgentId]
			boundCount[*t.AgentId]++
		}
		if act := byToken[t.Id.String()]; act != nil {
			c.Refusals7d = act.Refusals
		}
		inv.Credentials = append(inv.Credentials, c)
	}

	for _, a := range agents {
		row := InventoryAgent{
			ID:          a.Id,
			Name:        a.Name,
			IsActive:    a.IsActive,
			SponsorID:   a.CreatedBy,
			Sponsor:     names[a.CreatedBy],
			Brain:       brainLabel(a),
			Autonomy:    a.Autonomy,
			Trigger:     a.TriggerType,
			Channels:    len(a.ScopeConfig().ChannelIDs),
			Tools:       reachableTools(a),
			Credentials: boundCount[a.Id],
			LastRunAt:   a.LastRunAt,
		}
		if h := health[a.Id.String()]; h != nil {
			row.Runs7d = h.Last7dRuns
			if h.LastRunAt != nil {
				row.LastRunAt = h.LastRunAt
			}
		}
		if act := byAgent[a.Id.String()]; act != nil {
			row.Actions7d = act.Actions
			row.Refusals7d = act.Refusals
			row.LastRefusalAt = act.LastRefusalAt
		}
		inv.Agents = append(inv.Agents, row)
	}

	// What needs a look first: refused lately, then active before paused, then
	// the busiest. Stable so equal rows keep the list's newest-first order.
	sort.SliceStable(inv.Agents, func(i, j int) bool {
		a, b := inv.Agents[i], inv.Agents[j]
		if (a.Refusals7d > 0) != (b.Refusals7d > 0) {
			return a.Refusals7d > 0
		}
		if a.IsActive != b.IsActive {
			return a.IsActive
		}
		return a.Runs7d > b.Runs7d
	})
	return inv
}

// reachableTools is how many tools the agent can actually call. An A2A agent
// takes a task and answers in text; it is never offered a tool, whatever the
// agent was configured with before it pointed somewhere else.
func reachableTools(a *model.AiAgent) int {
	if a.AGUIEndpoint != "" && a.IsA2A() {
		return 0
	}
	return len(a.EnabledToolList())
}

// brainLabel says where an agent thinks: in this workspace, or at a remote
// endpoint named by protocol and host.
func brainLabel(a *model.AiAgent) string {
	if a.AGUIEndpoint == "" {
		return "workspace"
	}
	return remoteLabel(a)
}

func parseTokenScopes(raw string) []string {
	var s []string
	_ = json.Unmarshal([]byte(raw), &s)
	return s
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var errNothingToRevoke = errors.New("that credential is already revoked or expired")

// RevokeCredential lets an admin revoke any live credential from the
// inventory. A member revokes their own from their settings, as before.
func RevokeCredential(ctx context.Context, id uuid.UUID, actor Actor) (*tokenModel.ApiToken, error) {
	if !actor.IsAdmin {
		return nil, errForbidden
	}
	t, err := tokenModel.RevokeAny(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNothingToRevoke
	}
	return t, err
}
