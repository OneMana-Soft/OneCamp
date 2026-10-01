package business

// The agent card: what anyone in the workspace may know about an agent they
// meet.
//
// Agents now sit in channels and DMs next to people (Grok in X replies, Muse in
// Meta's apps), and the first thing a person asks of any participant is who it
// is and who answers for it. Until now OneCamp could only tell an admin or the
// agent's author, on the admin screens. Everyone else saw a bot name.
//
// So the card says, to any member: what the agent is for, who sponsors it,
// whether it acts on its own or asks first, what it is allowed to do and
// where, how busy it has been, and how often governance stopped it. That last
// number is the point: an agent that is refused is an agent that is being
// held to its sponsor's permissions, and members should be able to see that
// happening rather than take it on trust.
//
// What it does NOT say: instructions, model, credentials, remote endpoints,
// run transcripts or which channels by name. Those belong to the people who
// can manage the agent, and the card links them there (CanManage) instead of
// leaking any of it here.

import (
	"context"
	"strings"
	"time"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// cardWindow is how far back the card counts runs and refusals, the same week
// the admin inventory uses, so the two never disagree about "lately".
const cardWindow = model.InventoryWindow

// cardRunScan bounds how many recent runs are read to count refusals. A week
// of an unusually busy agent is well inside it; beyond it the count is a floor,
// which the card never presents as exact.
const cardRunScan = 200

type AgentCard struct {
	AgentID     uuid.UUID `json:"agent_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	// Sponsor is the person the agent acts for and whose permissions bound it.
	// Empty when the account cannot be resolved; the client says so in words.
	Sponsor  string `json:"sponsor,omitempty"`
	Active   bool   `json:"active"`
	DMable   bool   `json:"dm_able"`
	Autonomy string `json:"autonomy"`
	// Tools are the capability names it was granted; the client labels them.
	// Empty means its sponsor's full toolset, which is what the runner does.
	Tools []string `json:"tools"`
	// ScopedChannels and ScopedProjects count where it is confined to. Zero for
	// both means anywhere its sponsor can act. Counts, not names: a member may
	// not be able to see a channel the agent works in.
	ScopedChannels int `json:"scoped_channels"`
	ScopedProjects int `json:"scoped_projects"`
	// Runs, Actions and Refusals are for the last WindowDays days.
	Runs       int        `json:"runs"`
	Actions    int64      `json:"actions"`
	Refusals   int64      `json:"refusals"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	WindowDays int        `json:"window_days"`
	// CanManage: the reader is its sponsor or an admin, so the client may offer
	// pause and a link to the full record.
	CanManage bool `json:"can_manage"`
}

// Seams, so the card's rules can be tested without a database.
var (
	cardAgentByBot  = model.GetAgentByBotUser
	cardRuns        = model.ListRunsByAgent
	cardInventory   = model.InventoryActivity
	cardSponsorName = func(ctx context.Context, id uuid.UUID) string {
		u, err := userBusiness.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil {
			return ""
		}
		return memberName(u)
	}
	cardNow = time.Now
)

// GetAgentCard returns the card for the agent behind a bot user, or
// errNotFound when the bot is not an agent (OneCamp AI itself, an integration).
// Any signed-in member may read it; it holds nothing a member may not know.
func GetAgentCard(ctx context.Context, botUserID uuid.UUID, actor Actor) (*AgentCard, error) {
	a, err := cardAgentByBot(ctx, botUserID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errNotFound
	}
	now := cardNow()
	since := now.Add(-cardWindow)

	card := buildCard(a, actor)
	card.Sponsor = cardSponsorName(ctx, a.CreatedBy)
	card.WindowDays = int(cardWindow.Hours() / 24)

	// In-product refusals live in run transcripts; refusals over MCP (an outside
	// agent bound to this one) and completed actions live in the logs the
	// inventory already groups. Both are best effort: a card without a number is
	// still worth showing, and zero would be a claim.
	if runs, rerr := cardRuns(ctx, a.Id, cardRunScan); rerr == nil {
		countRuns(card, runs, since)
	}
	if byAgent, _, ierr := cardInventory(ctx, since); ierr == nil {
		if act := byAgent[a.Id.String()]; act != nil {
			card.Actions = act.Actions
			card.Refusals += act.Refusals
		}
	}
	return card, nil
}

// memberName is how a member is named to other members: their full name, else
// their display name, and never an email address. DisplayNamesByUUIDs falls
// back to the address, which suits the admin screens it was written for and
// put "maya.chen@cast.demo.onemana.dev" on a card every member can open. Pure.
func memberName(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return ""
	}
	for _, n := range []string{u.UserFullName, u.UserName} {
		if n = strings.TrimSpace(n); n != "" && !strings.Contains(n, "@") {
			return n
		}
	}
	return ""
}

// buildCard copies what a member may see from the agent row.
func buildCard(a *model.AiAgent, actor Actor) *AgentCard {
	scope := a.ScopeConfig()
	tools := a.EnabledToolList()
	if tools == nil {
		tools = []string{}
	}
	card := &AgentCard{
		AgentID:        a.Id,
		Name:           a.Name,
		Active:         a.IsActive,
		DMable:         a.DmAble,
		Autonomy:       a.Autonomy,
		Tools:          tools,
		ScopedChannels: len(scope.ChannelIDs),
		ScopedProjects: len(scope.ProjectIDs),
		CanManage:      canManage(actor, a),
	}
	if a.Description != nil {
		card.Description = strings.TrimSpace(*a.Description)
	}
	return card
}

// countRuns adds the window's runs, its newest run time and the governance
// refusals recorded in those runs' transcripts. Runs arrive newest first.
func countRuns(card *AgentCard, runs []*model.AgentRun, since time.Time) {
	for _, r := range runs {
		if r == nil || r.StartedAt.Before(since) {
			continue
		}
		card.Runs++
		if card.LastRunAt == nil || r.StartedAt.After(*card.LastRunAt) {
			t := r.StartedAt
			card.LastRunAt = &t
		}
		card.Refusals += int64(refusalsIn(parseSteps(r.Steps)))
	}
}

// ChannelAgent is an agent a member can ask in a channel: the start of a card.
type ChannelAgent struct {
	BotUserID   uuid.UUID `json:"bot_user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
}

// Seams for ChannelAgents.
var (
	channelMembersFor = func(ctx context.Context, channelUUID uuid.UUID, viewerDgraphUID string) (member bool, botUserIDs []uuid.UUID, err error) {
		ch, err := channelBusiness.GetDgraphChannelInfoByUUIDWithMemberAdminFlag(ctx, channelUUID, viewerDgraphUID)
		if err != nil || ch == nil {
			return false, nil, err
		}
		for _, m := range ch.Members {
			if m == nil || !m.IsBot {
				continue
			}
			if id, perr := uuid.Parse(m.Uuid); perr == nil {
				botUserIDs = append(botUserIDs, id)
			}
		}
		return ch.IsMember > 0, botUserIDs, nil
	}
	agentsByBots = model.ListActiveAgentsByBotUsers
)

// ChannelAgents lists the active agents that are members of a channel, for the
// people in it. An agent nobody knows is there is an agent nobody asks: the
// demo's only agent was found by visitors only because a seeded message
// happened to be written by it. Readable by channel members only, and it names
// nothing a member could not already see in the channel's roster.
func ChannelAgents(ctx context.Context, channelUUID uuid.UUID, actor Actor) ([]ChannelAgent, error) {
	member, bots, err := channelMembersFor(ctx, channelUUID, actor.DgraphUID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errForbidden
	}
	agents, err := agentsByBots(ctx, bots)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelAgent, 0, len(agents))
	for _, a := range agents {
		if a == nil || a.BotUserId == nil {
			continue
		}
		c := ChannelAgent{BotUserID: *a.BotUserId, Name: a.Name}
		if a.Description != nil {
			c.Description = strings.TrimSpace(*a.Description)
		}
		out = append(out, c)
	}
	return out, nil
}
