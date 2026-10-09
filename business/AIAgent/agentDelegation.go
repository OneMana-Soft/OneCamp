package business

// Agent-to-agent delegation: the budget that makes it safe for one AI teammate to
// hand work to another in a conversation.
//
// WHY THIS EXISTS
//
// Until now agents were structurally mute to each other. Every botpost write
// deliberately emits no post.created event, and the mention dispatcher is driven
// entirely by that event, so an agent's message could never reach another agent.
// That is a correct loop guard for one agent — and it is also the single thing
// blocking multi-agent collaboration, because it is a blanket mute rather than a
// bounded one.
//
// Removing the mute without a budget would be reckless: two agents that mention
// each other would ping-pong until the per-(agent,channel) queue cap sheds the
// flood or the daily token budget runs out. Both of those are backstops that fire
// AFTER money has been spent and a channel has been filled with noise. What is
// needed is a rule that refuses the second hop before it starts.
//
// THE MODEL
//
// A delegation chain is a walk: a human says something, an agent answers, that
// answer may mention another agent, and so on. Every link carries:
//
//   - Hop:      how many agent turns deep we already are. A human message is 0.
//   - OriginUserID: the PERSON at the root. Never empty for a dispatchable chain.
//     Every agent action must trace to someone who could have taken it themselves,
//     which is what makes permissions and audit meaningful rather than decorative.
//   - Chain:    the agent ids already in this walk, in order. Used for cycle
//     detection, and it is what the UI renders as lineage.
//
// The decision is a pure function of that context plus config. No DB, no clock,
// no network — so the interesting cases (cycles, self-delegation, exhausted hops,
// an orphaned chain with no human at the root) are all cheap to test exhaustively,
// which is the only way to be confident in a guard that gates spend.
//
// WHAT THIS DELIBERATELY IS NOT
//
// Not an orchestrator. There is no DAG, no plan, no coordinator process. The
// conversation is the orchestration surface: delegation happens by @mention, in
// the open, where a human can read it and interrupt it. A separate agent-DAG
// control plane would be a second source of truth about what the team is doing,
// invisible in the channel where the work is discussed.

import (
	"context"
	"os"
	"strconv"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// Event-payload keys carrying delegation lineage. These travel on the same
// workspace event map the mention dispatcher already reads, so no new transport
// is introduced — a chain is just three extra fields on post.created.
const (
	EventKeyAgentHop     = "agent_hop"
	EventKeyOriginUserID = "origin_user_id"
	EventKeyAgentChain   = "agent_chain"
)

// Defaults chosen to make the common useful case work and the pathological case
// impossible. Two hops covers "triage agent asks the code agent to open a PR"
// (human -> triage -> coder) which is the motivating example; it does not cover
// an open-ended relay, which is exactly the thing nobody asked for and which
// burns budget fastest.
const (
	defaultMaxDelegationHops = 2
	defaultMaxChainAgents    = 4
)

// DelegationDecision is the outcome of evaluating one proposed hop.
type DelegationDecision struct {
	Allow bool
	// Reason is always populated, including on Allow, so the decision can be
	// logged and shown in the lineage UI without a second lookup. A denial the
	// user cannot see is a denial they will report as a bug.
	Reason string
}

// DelegationConfig is the tunable part. Zero values mean "use the default", so a
// caller can pass an empty struct and get the safe behaviour.
type DelegationConfig struct {
	// Enabled gates the whole feature. When false, agent messages stay mute to
	// other agents exactly as before this file existed.
	Enabled bool
	// MaxHops is how many agent turns deep a chain may go. 0 => default.
	MaxHops int
	// MaxChainAgents bounds total distinct agents in one walk. 0 => default.
	MaxChainAgents int
	// ChannelOptIn reports whether this surface permits agent-to-agent work.
	// Per-CHANNEL rather than per-agent on purpose: an admin can open
	// #eng-triage for collaboration without opening every DM in the workspace,
	// and the blast radius of the setting is legible from the channel itself.
	ChannelOptIn bool
}

// DelegationContext is the state of the walk so far, read off the event payload.
type DelegationContext struct {
	Hop          int
	OriginUserID string
	Chain        []string
	// AuthorAgentID is the agent that produced the message being dispatched.
	// Empty means a human authored it, which is hop 0 and always allowed.
	AuthorAgentID string
}

// hopLimit / chainLimit resolve configured values against the defaults.
func (c DelegationConfig) hopLimit() int {
	if c.MaxHops > 0 {
		return c.MaxHops
	}
	return defaultMaxDelegationHops
}

func (c DelegationConfig) chainLimit() int {
	if c.MaxChainAgents > 0 {
		return c.MaxChainAgents
	}
	return defaultMaxChainAgents
}

// EvaluateDelegation decides whether targetAgentID may be dispatched for a
// message described by dc.
//
// Ordering matters and is deliberate: the cheapest and most absolute rules run
// first, so a denial never depends on a rule that could itself be misconfigured.
// Human-authored messages short-circuit to Allow before any agent rule is
// consulted, because this guard must not change existing single-agent behaviour.
func EvaluateDelegation(cfg DelegationConfig, dc DelegationContext, targetAgentID string) DelegationDecision {
	target := strings.TrimSpace(targetAgentID)
	if target == "" {
		return DelegationDecision{false, "no target agent"}
	}

	// A human speaking is not delegation. This is the path every existing
	// mention takes, and it must stay untouched whether the feature is on or off.
	if strings.TrimSpace(dc.AuthorAgentID) == "" {
		return DelegationDecision{true, "human-authored message"}
	}

	if !cfg.Enabled {
		return DelegationDecision{false, "agent-to-agent delegation is disabled"}
	}
	if !cfg.ChannelOptIn {
		return DelegationDecision{false, "this channel is not opted in to agent collaboration"}
	}

	// An agent-authored message with no human at the root is an orphan: nobody
	// authorised it, so there is no principal to permission-check against and no
	// one to hold accountable in an audit. Refuse rather than guess.
	if strings.TrimSpace(dc.OriginUserID) == "" {
		return DelegationDecision{false, "no originating person in the chain"}
	}

	// Self-delegation is always a loop, regardless of budget.
	if strings.EqualFold(dc.AuthorAgentID, target) {
		return DelegationDecision{false, "an agent cannot delegate to itself"}
	}

	// A cycle: this agent already acted in this walk. Allowing a revisit is how a
	// two-agent ping-pong becomes infinite while never exceeding a naive hop
	// count, because each side sees itself as starting fresh.
	for _, id := range dc.Chain {
		if strings.EqualFold(id, target) {
			return DelegationDecision{false, "agent already acted in this chain"}
		}
	}

	if dc.Hop >= cfg.hopLimit() {
		return DelegationDecision{false, "delegation hop budget exhausted"}
	}
	if len(dc.Chain) >= cfg.chainLimit() {
		return DelegationDecision{false, "too many agents in this chain"}
	}

	return DelegationDecision{true, "within delegation budget"}
}

// NextDelegationContext returns the lineage to stamp on the message the target
// agent is about to produce. Kept next to EvaluateDelegation because the two must
// agree about what a hop is: evaluate reads Hop, this writes Hop+1, and a
// mismatch would silently double or halve the effective budget.
//
// The returned Chain is a copy — appending to the caller's slice in place would
// let two concurrent dispatches from the same message corrupt each other's
// lineage, which is the classic append-aliasing bug and would be invisible until
// a chain reported the wrong parent.
func NextDelegationContext(dc DelegationContext, targetAgentID string) DelegationContext {
	chain := make([]string, 0, len(dc.Chain)+1)
	chain = append(chain, dc.Chain...)
	if t := strings.TrimSpace(targetAgentID); t != "" {
		chain = append(chain, t)
	}
	return DelegationContext{
		Hop:           dc.Hop + 1,
		OriginUserID:  dc.OriginUserID,
		Chain:         chain,
		AuthorAgentID: strings.TrimSpace(targetAgentID),
	}
}

// DelegationContextFromEvent reads lineage off a workspace event payload.
// Tolerant by construction: an event published before this feature existed, or by
// an older node during a rolling deploy, has none of these keys and must read as
// a human-authored hop 0 rather than as a malformed chain.
func DelegationContextFromEvent(data map[string]interface{}, authorAgentID string) DelegationContext {
	dc := DelegationContext{AuthorAgentID: strings.TrimSpace(authorAgentID)}
	if data == nil {
		return dc
	}
	switch v := data[EventKeyAgentHop].(type) {
	case int:
		dc.Hop = v
	case int64:
		dc.Hop = int(v)
	case float64: // survives a JSON round-trip through an outgoing webhook
		dc.Hop = int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			dc.Hop = n
		}
	}
	if dc.Hop < 0 {
		dc.Hop = 0
	}
	if s, ok := data[EventKeyOriginUserID].(string); ok {
		dc.OriginUserID = strings.TrimSpace(s)
	}
	dc.Chain = stringsFromEvent(data[EventKeyAgentChain])
	return dc
}

// ApplyToEvent stamps lineage onto an outgoing event payload. Mutates and returns
// data so it composes with the existing map literals at the dispatch sites.
func (dc DelegationContext) ApplyToEvent(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	data[EventKeyAgentHop] = dc.Hop
	if dc.OriginUserID != "" {
		data[EventKeyOriginUserID] = dc.OriginUserID
	}
	if len(dc.Chain) > 0 {
		data[EventKeyAgentChain] = dc.Chain
	}
	return data
}

// stringsFromEvent coerces a chain field that may arrive as []string (in-process
// listener) or []interface{} (after a JSON round-trip) into []string.
func stringsFromEvent(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, raw := range t {
			if s, ok := raw.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// AgentIDFromBotUser maps a bot principal back to the agent that owns it, using
// the same mention cache the dispatcher reads. Returns "" for a human author or
// an unknown principal — which EvaluateDelegation then treats as hop 0, the safe
// reading.
func AgentIDFromBotUser(authorUserID string) string {
	author := strings.TrimSpace(authorUserID)
	if author == "" {
		return ""
	}
	trigMu.RLock()
	defer trigMu.RUnlock()
	for _, a := range mentionCache {
		if a.BotUserId != nil && strings.EqualFold(a.BotUserId.String(), author) {
			return a.Id.String()
		}
	}
	for _, a := range ambientCache {
		if a.BotUserId != nil && strings.EqualFold(a.BotUserId.String(), author) {
			return a.Id.String()
		}
	}
	return ""
}

// delegationEnabled reports whether agent-to-agent delegation is on.
//
// The admin setting governs; the AI_AGENT_DELEGATION env var is a DEPLOYMENT-LEVEL
// KILL SWITCH that can only turn delegation OFF, never on. A self-hoster who has
// decided agents must never talk to each other can enforce that from
// infrastructure without depending on nobody flipping a toggle in the UI, and an
// admin cannot grant themselves a capability the deployment forbids.
//
// The asymmetry is the point: env absent means "no opinion, let the admin decide",
// not "enabled". Only an explicit false/0/no/off vetoes.
func delegationEnabled() bool {
	if envVetoesDelegation() {
		return false
	}
	enabled, _, _ := ai.AgentDelegationPolicy()
	return enabled
}

// envVetoesDelegation reports an explicit deployment-level refusal. Anything
// other than a recognised negative — including empty — is "no opinion".
// Defers to services/AI so the admin surface and the enforcement path read the
// same var through the same parse — two copies is how a UI ends up disagreeing
// with what the server actually does.
func envVetoesDelegation() bool {
	return ai.DelegationVetoedByEnv()
}

// delegationHopBudget prefers the admin setting, then
// AI_AGENT_DELEGATION_MAX_HOPS, then the default.
//
// Every layer falls back rather than failing closed-to-zero: a 0 budget would
// silently disable a feature the operator had turned on, which is the one outcome
// that looks like a bug rather than a setting.
func delegationHopBudget() int {
	if _, hops, _ := ai.AgentDelegationPolicy(); hops > 0 {
		return hops
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AI_AGENT_DELEGATION_MAX_HOPS"))); err == nil && n > 0 {
		return n
	}
	return defaultMaxDelegationHops
}

// agentCollabAllowedInChannel reports whether a channel permits agent-to-agent
// work, from AI_AGENT_DELEGATION_CHANNELS: a comma-separated list of channel ids,
// or "*" for every channel.
//
// Deployment-level rather than a channel column on purpose, for now. Adding a
// migration and an admin toggle before the feature has been exercised would ship
// a schema change and a settings control for behaviour nobody has run yet; an env
// allowlist lets an operator enable one channel, watch it, and widen. The channel
// column and its admin UI replace this once the shape is proven — the config
// struct already takes ChannelOptIn as a plain bool so that swap touches only
// this function.
// The allowlist comes from the admin setting, falling back to
// AI_AGENT_DELEGATION_CHANNELS only when the admin has named nothing. The env var
// is therefore a bootstrap for a deployment that wants delegation working before
// anyone opens the admin UI, not a second competing source: once an admin names a
// surface, that list is authoritative and the env var stops being consulted, so
// there is never a question of which one is in force.
func agentCollabAllowedInSurface(surfaceKey string) bool {
	_, _, configured := ai.AgentDelegationPolicy()
	raw := strings.TrimSpace(configured)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("AI_AGENT_DELEGATION_CHANNELS"))
	}
	if raw == "" {
		return false
	}
	if raw == "*" {
		return true
	}
	id := strings.TrimSpace(surfaceKey)
	if id == "" {
		return false
	}
	for _, allowed := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(allowed), id) {
			return true
		}
	}
	return false
}

// delegationSurfaceKey is the id an operator names in the allowlist, and the same
// id the permission check is asked about. One function so the thing being opted in
// and the thing being authorised can never be different things — a channel opted
// in by id must not accidentally authorise a task that merely shares it.
//
// A task is keyed "task:<uuid>" rather than by bare uuid so the two id spaces
// cannot collide in a flat, human-edited list.
// Ids are validated as uuids here, not merely trimmed. A malformed id cannot
// identify a real surface, so it cannot be opted in and cannot be authorised —
// and refusing it at the key means the permission check spends no queries
// discovering that.
func delegationSurfaceKey(where Surface, entityID string) string {
	switch where.Kind {
	case SurfaceChannelPost:
		if id, ok := ParseAgentUUID(where.ChannelID); ok {
			return id.String()
		}
		return ""
	case SurfaceTask:
		if id, ok := ParseAgentUUID(entityID); ok {
			return "task:" + id.String()
		}
		return ""
	default:
		// A surface this does not understand (group chat, DM) cannot be opted in,
		// so delegation there is off until it is taught here — the same
		// deny-by-default posture as the visibility check.
		return ""
	}
}

// LoadDelegationConfig assembles the config for one dispatch. channelOptIn is
// supplied by the caller because it comes from channel state, keeping this file
// free of DB access and therefore fully unit-testable.
func LoadDelegationConfig(channelOptIn bool) DelegationConfig {
	return DelegationConfig{
		Enabled:        delegationEnabled(),
		MaxHops:        delegationHopBudget(),
		MaxChainAgents: defaultMaxChainAgents,
		ChannelOptIn:   channelOptIn,
	}
}

// ParseAgentUUID is a small helper for callers that hold a string id.
func ParseAgentUUID(id string) (uuid.UUID, bool) {
	u, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, false
	}
	return u, true
}

// ---------------------------------------------------------------------------
// Emission: making an agent's message audible to other agents.
// ---------------------------------------------------------------------------
//
// EventTypeAgentMessage is a DEDICATED internal event rather than a reuse of
// post.created, and that choice is the whole safety story of this feature.
//
// Reusing post.created would have been fewer lines and would also have started
// fanning every agent reply out to configured OUTGOING WEBHOOKS and to the
// Workflow engine — neither of which asked for it, both of which could react to
// it, and the workflow engine deliberately avoids emitting for exactly this
// reason. A new type is inert for anything that does not subscribe to it: the
// outgoing-webhook dispatcher matches on configured event types, and every
// in-process listener filters by type. So the blast radius of turning this on is
// exactly one code path — the agent mention dispatcher.
const EventTypeAgentMessage = "agent.message"

type delegationCtxKey struct{}

// WithDelegationContext attaches the lineage an agent's own message should carry.
// Passed through ctx rather than threaded as a parameter because the reply is
// written several frames below the dispatcher (launch -> run -> post, with a
// durable-queue detour in between); widening five signatures to carry three
// fields would be a worse trade than one context value, and the codebase already
// uses this pattern for agent resume state.
func WithDelegationContext(ctx context.Context, dc DelegationContext) context.Context {
	return context.WithValue(ctx, delegationCtxKey{}, dc)
}

// delegationFromContext reads the lineage back. A zero value means "not part of a
// delegation chain", which is the correct reading for every path that does not
// set it (schedules, routines, manual runs).
func delegationFromContext(ctx context.Context) (DelegationContext, bool) {
	dc, ok := ctx.Value(delegationCtxKey{}).(DelegationContext)
	return dc, ok
}

// EmitAgentMessage publishes an agent's own message as an agent.message event so
// other agents can be @mentioned by it.
//
// Silent no-op unless delegation is enabled AND this channel is opted in, so with
// the feature off this function costs one env read and changes nothing observable
// — agents stay mute to each other exactly as before.
//
// Best-effort by design: a failure to emit means a delegation does not happen,
// which is strictly safer than the write having failed, so it is logged and
// swallowed rather than propagated into the agent's reply path.
// where identifies the surface the message landed on. Delegation must work on
// EVERY surface an agent speaks on, not just the one that was easiest to wire: a
// tagged agent answers in a channel THREAD, and an agent assigned a task answers
// in a task COMMENT. Taking the surface rather than a channel id is what keeps
// those two from needing separate implementations that drift.
func EmitAgentMessage(ctx context.Context, where Surface, entityID, channelName, postID, htmlText string, bot *userBusiness.BotIdentity) {
	optInKey := delegationSurfaceKey(where, entityID)
	if !delegationEnabled() || !agentCollabAllowedInSurface(optInKey) {
		return
	}
	if bot == nil || strings.TrimSpace(bot.UUID) == "" || optInKey == "" {
		return
	}
	// No mentions means nothing to delegate to; skip the fan-out entirely rather
	// than waking every listener for a message no agent is named in.
	mentionIDs, _ := helpers.GetMentions(htmlText)
	if len(mentionIDs) == 0 {
		return
	}

	dc, ok := delegationFromContext(ctx)
	if !ok {
		// An agent message with no lineage on the context is not part of a chain
		// we vouched for (a schedule, a routine, a manual run). Emitting it would
		// create a chain with no originating person, which EvaluateDelegation
		// refuses anyway — so stop here and save the fan-out.
		return
	}

	payload := map[string]interface{}{
		"post_id":      postID,
		"channel_id":   strings.TrimSpace(where.ChannelID),
		"channel_name": channelName,
		// The surface travels with the event so the dispatcher can route a reply
		// back to the same place the delegation happened, rather than assuming
		// every chain lives in a channel.
		"surface_kind":      string(where.Kind),
		"surface_entity_id": strings.TrimSpace(entityID),
		"text":              helpers.HTMLToPlainText(htmlText),
		"author_id":         bot.UUID,
		"author_name":       bot.Name,
		"mention_ids":       mentionIDs,
		"source":            "agent",
	}
	dc.ApplyToEvent(payload)

	// Detached: the emitting agent's run must not be blocked by, or cancelled
	// into, the downstream agent's dispatch.
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), EventTypeAgentMessage, payload)
}

// ---------------------------------------------------------------------------
// Permissions: delegation must not launder authority.
// ---------------------------------------------------------------------------
//
// An agent's tools execute as its OWN owner (agentRunner: userUUID =
// agent.CreatedBy), but a run is FOR the person who asked, and reaches only
// what both the owner and that person can (agentRequester.go). That intersection
// is enforced on every tool call, every search and every list.
//
// It used to rest on an argument instead: to mention an agent you must be able
// to post in a channel the agent is scoped to, so the authority a person had
// over an agent was exactly "we share a channel". That never held. A DM to a
// dm_able agent needs no shared channel, an unscoped agent answers in any
// channel, and the tools were never confined to the channel anyway: search,
// other channels, DMs, docs, tasks and the owner's connected accounts all ran
// with the owner's reach for whoever asked. So the asker's reach is now checked
// directly rather than inferred from where they spoke.
//
// Delegation is covered by the same rule, because a chain carries its origin.
// Alice, a limited member, mentions @triage (owned by an admin); @triage's reply
// mentions @coder (owned by another admin). Each hop is a run FOR Alice
// (dispatchMentionAgents credits rooted.OriginUserID, and a durable hop records
// her as triggered_by), so @coder's tools reach only what Alice and @coder's
// owner can both reach. A chain Alice started can never execute with authority
// Alice never had.
//
// The check below answers the question that remains: may the originating person
// address this agent on this surface at all? A DELEGATED RUN MAY NEVER REACH AN
// AGENT THE ORIGINATING PERSON COULD NOT HAVE ADDRESSED THEMSELVES. It is a
// pre-flight check on the hop, cheaper than starting a run whose every call
// would then be refused, and it keeps a chain out of a channel its origin
// cannot see even when nothing in that run would read anything.
//
// Today the chain happens to stay in the channel the human posted in, so
// membership holds BY CONSTRUCTION. That is exactly why this is checked
// explicitly: the moment someone wires emission into the send_message executor,
// an agent could carry a chain into a channel the origin cannot see, and a
// construction-based argument fails silently. A guard that only works because of
// a property nobody wrote down is a guard waiting to break.
//
// Conservative by construction: anything that cannot be verified is a refusal, so
// a surface this cannot reason about denies delegation rather than permitting it.
func originCanAddressSurface(ctx context.Context, originUserID string, where Surface, entityID string) (allowed bool) {
	// FAIL CLOSED, NEVER CRASH. The visibility lookups below reach a graph client
	// that is nil until the server has connected, and the driver dereferences it
	// without checking — so asking this question during a datastore blip panics
	// rather than returning an error, inside a dispatcher goroutine.
	//
	// A refusal is the only safe failure for an authorization question, and it is
	// also the only safe failure for AVAILABILITY: an outage should degrade to "no
	// agent can act", never to "the process still serving humans dies". A guard that
	// can crash the server it protects is not a guard.
	//
	// Named return so the recover can set it; false is the zero value, so even an
	// unexpected panic path denies.
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf(
				"agentDelegation: permission lookup panicked, refusing delegation: %v", r)
			allowed = false
		}
	}()

	origin := strings.TrimSpace(originUserID)
	if origin == "" {
		return false
	}
	// Validate the surface BEFORE any lookup. An unkeyable surface — no channel
	// id, no entity id, or a kind this cannot reason about — is a refusal on its
	// own terms, and resolving a user first would spend two queries to reach the
	// same answer on the message hot path.
	if delegationSurfaceKey(where, entityID) == "" {
		return false
	}
	// The lineage carries the business user uuid (the event's author_id); both
	// visibility queries need the graph node uid.
	dgraphUser, uerr := userDomain.GetDgraphUserInfoByUUID(ctx, origin)
	if uerr != nil {
		return false
	}

	// MAY THIS IDENTITY AUTHORIZE A CHAIN AT ALL? Separate from what it can see.
	//
	// Resolving the origin proves the node exists, not that the person behind it
	// still works here. Deactivation writes a timestamp and leaves every membership
	// edge intact, so the visibility queries below happily confirm a chain rooted
	// at someone offboarded months ago — their scheduled and ambient agents keep
	// running against surfaces they can no longer open themselves.
	//
	// It also enforces what originLineage only arranges. That function is careful
	// never to overwrite an inherited origin, because otherwise "every chain would
	// look like it was authorised by a bot"; this makes a chain that does root at a
	// bot a refusal instead of a silent audit gap.
	//
	// Shared with the MCP surface (business/MCPServer.PrincipalCanReach) rather
	// than restated, so the two cannot drift into disagreeing about who counts as a
	// person.
	if eligible := principalBusiness.Assess(dgraphUser); !eligible.Allowed {
		return false
	}

	switch where.Kind {
	case SurfaceChannelPost:
		channelUUID, err := uuid.Parse(strings.TrimSpace(where.ChannelID))
		if err != nil {
			return false
		}
		// Same visibility query agentWorkEntity uses for "can this person see the
		// thread an agent is working in". Reused deliberately: a second permission
		// model for the same question is how the two drift apart.
		info, cerr := channelBusiness.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelUUID, dgraphUser.Uid, "")
		if cerr != nil || info == nil {
			return false
		}
		// Deletion does not clear membership edges, so IsMember alone still
		// reports a member of a channel that no longer exists.
		if helpers.IsSoftDeleted(info.DeletedAt) {
			return false
		}
		return info.IsMember > 0

	case SurfaceTask:
		// Project membership is what grants sight of a task, exactly as
		// canSeeWorkSurface decides it for the work feed.
		id := strings.TrimSpace(entityID)
		if id == "" {
			return false
		}
		info, terr := taskBusiness.GetDgraphTaskInfo(ctx, id, dgraphUser.Uid)
		if terr != nil || info == nil || info.Project == nil {
			return false
		}
		// Same reasoning as the channel case, for both objects: a deleted task and
		// an archived project both leave the project's membership edges intact.
		if helpers.IsSoftDeleted(info.DeletedAt) || helpers.IsSoftDeleted(info.Project.DeletedAt) {
			return false
		}
		return info.Project.IsProjectMember > 0

	default:
		// Group chat / DM: this package holds no chat dependency by design, so
		// membership cannot be proven here. Refuse rather than assume.
		return false
	}
}

// AuthorizeDelegation is the full gate for one proposed hop: the budget rules
// first (pure, cheap, no I/O), then the permission check, which costs two queries
// and therefore runs only for a hop that would otherwise be allowed.
//
// Returns the decision so callers log one reason. Human-authored messages never
// reach the permission check — EvaluateDelegation short-circuits them — so a
// normal mention costs nothing.
func AuthorizeDelegation(ctx context.Context, cfg DelegationConfig, dc DelegationContext, targetAgentID string, where Surface, entityID string) DelegationDecision {
	if d := EvaluateDelegation(cfg, dc, targetAgentID); !d.Allow {
		return d
	}
	// A human's own mention is authorised by the fact they posted it.
	if strings.TrimSpace(dc.AuthorAgentID) == "" {
		return DelegationDecision{true, "human-authored message"}
	}
	if !originCanAddressSurface(ctx, dc.OriginUserID, where, entityID) {
		return DelegationDecision{false, "the originating person cannot act on this surface"}
	}
	return DelegationDecision{true, "within delegation budget and authorised by the originating person"}
}

// emitAgentFinalAnswer publishes a durable run's clean final answer so another
// agent it @mentions can pick the work up.
//
// Bridges the durable worker (which holds a task row) to EmitAgentMessage (which
// speaks in surfaces), and resolves the two things the worker does not carry: the
// agent's own bot principal, and the lineage. The lineage is rebuilt from the task
// row rather than read off the context because a durable job crosses a process
// boundary — it was enqueued by one goroutine and is executed later, possibly on
// another node, so the dispatcher's context is long gone.
//
// triggered_by IS the durable chain's memory: the previous commits made it the
// ORIGINATING PERSON, so a hop executed hours later still knows which human
// authorised it. What a task row cannot recover is the agent chain, so hop is
// treated as 1 and the chain as just this agent. That is deliberately
// conservative: a rebuilt chain cannot detect a cycle it has forgotten, so the hop
// budget is what stops a durable ping-pong, and it must be the tighter of the two
// bounds rather than the looser.
// Takes primitives rather than a task row so it depends on no particular row
// shape — the worker holds an AgentTask, the work feed an AgentActiveTask, and a
// future caller may hold neither.
// hop and chain are the lineage PERSISTED on the job (migration 137). They are
// read from the row rather than the context because a durable job crosses a
// process boundary: it was enqueued by one goroutine and runs later, possibly on
// another node, so the dispatcher's context is long gone.
func emitAgentFinalAnswer(ctx context.Context, agent *model.AiAgent, where Surface, sourceID, origin string, hop int, chain []string, body string) {
	if agent == nil || strings.TrimSpace(body) == "" {
		return
	}
	if !delegationEnabled() {
		return // cheap exit before resolving a principal or touching the surface
	}
	if strings.TrimSpace(origin) == "" {
		// No human at the root (a scheduled routine). EvaluateDelegation would
		// refuse this chain anyway, so stop before doing any work for it.
		return
	}
	bot := resolveAgentBot(ctx, agent)
	if bot == nil {
		return // cannot be attributed to one agent, so it cannot delegate
	}

	// Advance the persisted lineage by this agent's turn. NextDelegationContext is
	// the same function the synchronous path uses, so both advance a chain
	// identically — a second implementation here is how the two budgets would
	// silently diverge.
	//
	// A job with no stored chain (enqueued before migration 137, or by a schedule)
	// falls back to hop 1 with this agent alone: the conservative reading, and the
	// same one the worker used before the lineage was persisted.
	lineage := NextDelegationContext(DelegationContext{
		Hop:          hop,
		OriginUserID: strings.TrimSpace(origin),
		Chain:        chain,
	}, agent.Id.String())
	EmitAgentMessage(WithDelegationContext(ctx, lineage), where, delegationEntityID(where, sourceID), "", where.PostID, body, bot)
}

// delegationEntityID resolves the surface entity a job is attached to from its
// source id, mirroring workEntityID's prefix handling ("task:", "post:", "msg:")
// so a coding job opened from a thread resolves to the same entity the work feed
// reports. The surface descriptor wins where it carries the id itself.
func delegationEntityID(where Surface, sourceID string) string {
	if id := strings.TrimSpace(where.PostID); id != "" && where.Kind == SurfaceChannelPost {
		return id
	}
	id := strings.TrimSpace(sourceID)
	for _, prefix := range []string{"task:", "post:", "msg:"} {
		id = strings.TrimPrefix(id, prefix)
	}
	return id
}
