package ai

// Two-tier AI token budget, enforced at the provider chokepoint so EVERY AI
// feature (assistant chat, Board AI, agents, tables, workflows, doc/in-call
// assistants, ...) shares one accounting path:
//
//   - Workspace daily cap (AI_WORKSPACE_DAILY_TOKEN_BUDGET): bounds total
//     spend across the whole workspace per UTC day.
//   - Per-user daily cap (AI_USER_DAILY_TOKEN_BUDGET): stops a single member
//     from burning the workspace budget on their own.
//
// A call is refused if EITHER cap is already exceeded; every completion
// increments BOTH meters. Both are best-effort (fail-open): a Redis outage
// never blocks AI, since the agent runner's per-run cap and the provider's
// per-call MaxTokens remain as backstops. Enforcement gates on raw token
// counts (provider-agnostic); USD cost stays informational (see cost.go).

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// BudgetScope identifies which cap a decision applies to.
type BudgetScope string

const (
	BudgetScopeNone      BudgetScope = ""
	BudgetScopeWorkspace BudgetScope = "workspace"
	BudgetScopeUser      BudgetScope = "user"
	BudgetScopeAgent     BudgetScope = "agent"
	BudgetScopeChannel   BudgetScope = "channel"
)

// budgetDimension is one extra daily-metered budget axis layered onto the
// always-present workspace + per-user meters. It is fully generic: the agent
// runner adds an "agent" dimension, channel-scoped AI adds a "channel" one, and
// any future axis (team, project) is one more dimension with no new plumbing.
// Spend records against every dimension; a call is refused when a dimension
// with Limit>0 is already at/over its cap. Limit 0 meters without capping (so
// reporting works even when no cap is set).
type budgetDimension struct {
	Scope BudgetScope   // which cap, for the refusal message/error
	Spec  registry.Spec // the redis counter family (arity 2: id + day)
	ID    string        // the axis identity (agentID / channelID)
	Limit int           // daily token cap (0 = meter only, never refuse)
	// Leaderboard, when set, is a per-day sorted set the spend is also ZINCRBY'd
	// into, so an admin can see the top spenders on this axis (e.g. top channels)
	// without scanning the per-id counters. Zero value = no leaderboard.
	Leaderboard registry.Spec
}

type aiDimsKeyT struct{}

var aiDimsKey aiDimsKeyT

// WithBudgetDimension layers an extra daily budget meter onto ctx (e.g. a
// specific AI teammate or a channel). Generic and composable: call it more than
// once to stack dimensions. A blank id is a no-op. Used by the agent runner
// (per-agent cap, billing teammate work to the agent rather than the owner's
// personal seat) and channel-scoped AI (per-channel cap).
func WithBudgetDimension(ctx context.Context, scope BudgetScope, spec registry.Spec, id string, limit int) context.Context {
	return withBudgetDimension(ctx, budgetDimension{Scope: scope, Spec: spec, ID: id, Limit: limit})
}

// withBudgetDimension appends a fully-formed dimension (including an optional
// leaderboard) without aliasing the parent's slice.
func withBudgetDimension(ctx context.Context, d budgetDimension) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.ID == "" {
		return ctx
	}
	existing := budgetDimensions(ctx)
	dims := make([]budgetDimension, 0, len(existing)+1)
	dims = append(dims, existing...)
	dims = append(dims, d)
	return context.WithValue(ctx, aiDimsKey, dims)
}

func budgetDimensions(ctx context.Context) []budgetDimension {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(aiDimsKey).([]budgetDimension); ok {
		return v
	}
	return nil
}

// AgentBudgetID returns the agent id metered on ctx (set by WithAgentBudget when
// an AI call runs as an agent teammate), or "" when none. Lets a tool executor
// recover the acting agent's identity from the run context without new plumbing
// — e.g. to enqueue a durable job attributed to that agent.
func AgentBudgetID(ctx context.Context) string {
	for _, d := range budgetDimensions(ctx) {
		if d.Scope == BudgetScopeAgent {
			return d.ID
		}
	}
	return ""
}

// ChannelBudgetID returns the channel id metered on ctx (set by
// WithChannelBudget when an AI call runs inside a channel), or "" when none.
// It lets non-budget callers reuse the run's channel scope without re-plumbing
// it — e.g. to gather that channel's recent discussion as grounding context for
// the code agent — so scope threading has one source of truth.
func ChannelBudgetID(ctx context.Context) string {
	for _, d := range budgetDimensions(ctx) {
		if d.Scope == BudgetScopeChannel {
			return d.ID
		}
	}
	return ""
}

// WithAgentBudget bills an AI teammate's spend to its OWN per-agent daily budget
// (and enforces dailyLimit, 0 = no cap) instead of any human's seat quota. The
// thin, intent-named wrapper the agent runner uses so the business layer never
// touches the redis registry directly.
func WithAgentBudget(ctx context.Context, agentID string, dailyLimit int) context.Context {
	return WithBudgetDimension(ctx, BudgetScopeAgent, registry.AIAgentTokenBudget, agentID, dailyLimit)
}

// WithChannelBudget meters (and optionally caps) AI spend incurred within a
// channel, for Claude-Tag-style channel-level cost control. dailyLimit 0 meters
// without capping.
func WithChannelBudget(ctx context.Context, channelID string, dailyLimit int) context.Context {
	return withBudgetDimension(ctx, budgetDimension{
		Scope:       BudgetScopeChannel,
		Spec:        registry.AIChannelTokenBudget,
		ID:          channelID,
		Limit:       dailyLimit,
		Leaderboard: registry.AIChannelTokenLeaderboard,
	})
}

// aiActorKey carries the user UUID an AI call should be metered against, when a
// caller (e.g. the agent runner) sets it explicitly. HTTP callers don't need to
// set it: actorFromContext falls back to the authenticated UserInfo that auth
// middleware already put on the request context.
type aiActorKeyT struct{}

var aiActorKey aiActorKeyT

// WithActor tags ctx with the user the AI spend should be attributed to. Used by
// background callers (the agent runner attributes to the agent's owner) that
// don't carry an authenticated UserInfo. No-op for a blank id.
func WithActor(ctx context.Context, userUUID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if userUUID == "" {
		return ctx
	}
	return context.WithValue(ctx, aiActorKey, userUUID)
}

// actorFromContext resolves the user UUID to meter, preferring an explicit
// WithActor tag and falling back to the authenticated UserInfo set by auth
// middleware. Returns "" when no user can be determined (then only the
// workspace cap applies).
func actorFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, _ := ctx.Value(aiActorKey).(string); v != "" {
		return v
	}
	if ui, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo); ok {
		if id := ui.UserPostgresInfo.Id; id != (userModel.User{}).Id {
			return id.String()
		}
	}
	return ""
}

// Budget caps are resolved from the live, hot-reloaded AI config (set by an
// admin in the panel). The AI_*_DAILY_TOKEN_BUDGET env vars remain a boot
// fallback, used only until an admin sets a value (config value 0 = "unset").
func workspaceTokenBudget() int {
	if c := GetConfig(); c != nil && c.WorkspaceDailyTokenBudget > 0 {
		return c.WorkspaceDailyTokenBudget
	}
	return envInt("AI_WORKSPACE_DAILY_TOKEN_BUDGET")
}

func userTokenBudget() int {
	if c := GetConfig(); c != nil && c.UserDailyTokenBudget > 0 {
		return c.UserDailyTokenBudget
	}
	return envInt("AI_USER_DAILY_TOKEN_BUDGET")
}

// budgetDayKey is the UTC day bucket shared by both meters.
func budgetDayKey() string { return time.Now().UTC().Format("20060102") }

// TokenBudgetExceeded reports whether the next AI call should be refused because
// a daily cap is already reached, and which cap. Checks the per-user cap first
// (more specific) then the workspace cap. Fail-open: returns (false, none) when
// no cap is configured or Redis is unavailable.
func TokenBudgetExceeded(ctx context.Context) (bool, BudgetScope) {
	if ub := userTokenBudget(); ub > 0 {
		if actor := actorFromContext(ctx); actor != "" {
			if count, found, err := redisStore.GetInt64(ctx, registry.AIUserTokenBudget, []string{actor, budgetDayKey()}); err == nil && found && count >= int64(ub) {
				return true, BudgetScopeUser
			}
		}
	}

	// Extra dimensions (per-agent, per-channel, ...) are more specific than the
	// workspace cap, so they are checked before it. Each is independent.
	for _, d := range budgetDimensions(ctx) {
		if d.Limit <= 0 || d.ID == "" {
			continue
		}
		if count, found, err := redisStore.GetInt64(ctx, d.Spec, []string{d.ID, budgetDayKey()}); err == nil && found && count >= int64(d.Limit) {
			return true, d.Scope
		}
	}

	if wb := workspaceTokenBudget(); wb > 0 {
		if count, found, err := redisStore.GetInt64(ctx, registry.AITokenBudget, []string{budgetDayKey()}); err == nil && found && count >= int64(wb) {
			return true, BudgetScopeWorkspace
		}
	}

	return false, BudgetScopeNone
}

// RecordTokenSpend adds consumed tokens to the workspace meter and, when the
// call can be attributed to a user, that user's meter too. Best-effort.
func RecordTokenSpend(ctx context.Context, tokens int) {
	if tokens <= 0 {
		return
	}
	day := budgetDayKey()
	// Always meter the workspace so the cap and reporting are accurate even
	// when no per-user cap is set.
	_, _ = redisStore.IncrByWithTTL(ctx, registry.AITokenBudget, []string{day}, int64(tokens))
	if actor := actorFromContext(ctx); actor != "" {
		_, _ = redisStore.IncrByWithTTL(ctx, registry.AIUserTokenBudget, []string{actor, day}, int64(tokens))
		// Also fold into the per-day leaderboard so an admin can see today's
		// top consumers without SCANning the per-user counter keyspace.
		redisStore.ZIncrBy(ctx, registry.AITokenUserLeaderboard, []string{day}, actor, float64(tokens))
	}
	// Meter every extra dimension (agent, channel, ...) so its cap and usage
	// breakdown stay accurate even when no cap is set (Limit 0 still meters).
	for _, d := range budgetDimensions(ctx) {
		if d.ID == "" {
			continue
		}
		_, _ = redisStore.IncrByWithTTL(ctx, d.Spec, []string{d.ID, day}, int64(tokens))
		if d.Leaderboard.Namespace != "" {
			redisStore.ZIncrBy(ctx, d.Leaderboard, []string{day}, d.ID, float64(tokens))
		}
	}
}

// UserTokenSpend is one user's token spend for the current day, for the admin
// "top consumers" view. Name is resolved by the caller (the service layer
// stays out of the user model).
type UserTokenSpend struct {
	UserID string `json:"user_id"`
	Used   int64  `json:"used"`
}

// TopUserUsage returns today's highest AI-token consumers (most first), read
// from the per-day leaderboard sorted set. Read-only and best-effort: an empty
// slice when Redis is unavailable. limit <= 0 defaults to 20.
func TopUserUsage(ctx context.Context, limit int) []UserTokenSpend {
	rows := redisStore.ZRevRangeWithScores(ctx, registry.AITokenUserLeaderboard, []string{budgetDayKey()}, limit)
	out := make([]UserTokenSpend, 0, len(rows))
	for _, r := range rows {
		if r.Member == "" {
			continue
		}
		out = append(out, UserTokenSpend{UserID: r.Member, Used: int64(r.Score)})
	}
	return out
}

// AgentTokenUsageToday reads one agent's AI token spend for the current UTC day
// (read-only, best-effort: 0 on a Redis miss). Powers the per-agent usage/cap
// display in the builder and admin panel.
func AgentTokenUsageToday(ctx context.Context, agentID string) int64 {
	if agentID == "" {
		return 0
	}
	if count, found, err := redisStore.GetInt64(ctx, registry.AIAgentTokenBudget, []string{agentID, budgetDayKey()}); err == nil && found {
		return count
	}
	return 0
}

// ChannelTokenUsageToday reads one channel's AI token spend for the current UTC
// day (read-only, best-effort). Powers the per-channel usage/cap display.
func ChannelTokenUsageToday(ctx context.Context, channelID string) int64 {
	if channelID == "" {
		return 0
	}
	if count, found, err := redisStore.GetInt64(ctx, registry.AIChannelTokenBudget, []string{channelID, budgetDayKey()}); err == nil && found {
		return count
	}
	return 0
}

// ChannelTokenSpend is one channel's AI token spend for the current day, for
// the admin "top AI-spending channels" view.
type ChannelTokenSpend struct {
	ChannelID string `json:"channel_id"`
	Used      int64  `json:"used"`
}

// TopChannelUsage returns today's highest AI-spending channels (most first),
// read from the per-day channel leaderboard. Read-only and best-effort: an
// empty slice when Redis is unavailable. limit <= 0 defaults to 20.
func TopChannelUsage(ctx context.Context, limit int) []ChannelTokenSpend {
	rows := redisStore.ZRevRangeWithScores(ctx, registry.AIChannelTokenLeaderboard, []string{budgetDayKey()}, limit)
	out := make([]ChannelTokenSpend, 0, len(rows))
	for _, r := range rows {
		if r.Member == "" {
			continue
		}
		out = append(out, ChannelTokenSpend{ChannelID: r.Member, Used: int64(r.Score)})
	}
	return out
}

// GuardTokenBudget is the exported early-check for heavy AI features that do
// expensive work (repo retrieval, large prompt assembly) BEFORE reaching the
// provider chokepoint. It returns the same typed, tier-specific budget error
// (user / agent / channel / workspace) the chokepoint would, so an expensive
// operation can bail out early on the already-configured limits instead of
// doing the work and getting refused at Chat. Fail-open (nil) when no cap is
// set or Redis is down, exactly like the chokepoint guard.
func GuardTokenBudget(ctx context.Context) error { return guardTokenBudget(ctx) }

// guardTokenBudget is the provider-layer entry check: it returns a user-safe
// error when a daily cap is already exceeded, or nil to proceed.
func guardTokenBudget(ctx context.Context) error {
	exceeded, scope := TokenBudgetExceeded(ctx)
	if !exceeded {
		return nil
	}
	switch scope {
	case BudgetScopeUser:
		return ErrUserTokenBudgetExceeded
	case BudgetScopeAgent:
		return ErrAgentTokenBudgetExceeded
	case BudgetScopeChannel:
		return ErrChannelTokenBudgetExceeded
	default:
		return ErrWorkspaceTokenBudgetExceeded
	}
}

// estimateMessagesTokens sums the calibrated token estimate over a set of
// messages, used to meter streaming calls whose providers don't return exact
// usage.
func estimateMessagesTokens(messages []ChatMessage) int {
	n := 0
	for _, m := range messages {
		n += EstimateTokens(m.Content)
	}
	return n
}

// MeterUsage is one budget meter's state for the current day.
type MeterUsage struct {
	Used  int64 `json:"used"`  // tokens spent today
	Limit int   `json:"limit"` // configured daily cap (0 = unlimited)
}

// UsageSnapshot is today's AI token usage for the workspace and (when the call
// is attributable to a user) that user, with their configured caps. Powers the
// admin panel's usage display and a user's "you're near your limit" hint.
type UsageSnapshot struct {
	Day       string     `json:"day"` // UTC YYYYMMDD bucket
	Workspace MeterUsage `json:"workspace"`
	User      MeterUsage `json:"user"`
}

// CurrentUsage reads today's workspace and per-user token spend plus the
// configured caps. Read-only and best-effort: a Redis miss reports 0 used.
func CurrentUsage(ctx context.Context) UsageSnapshot {
	day := budgetDayKey()
	snap := UsageSnapshot{
		Day:       day,
		Workspace: MeterUsage{Limit: workspaceTokenBudget()},
		User:      MeterUsage{Limit: userTokenBudget()},
	}
	if count, found, err := redisStore.GetInt64(ctx, registry.AITokenBudget, []string{day}); err == nil && found {
		snap.Workspace.Used = count
	}
	if actor := actorFromContext(ctx); actor != "" {
		if count, found, err := redisStore.GetInt64(ctx, registry.AIUserTokenBudget, []string{actor, day}); err == nil && found {
			snap.User.Used = count
		}
	}
	return snap
}

// AgentBudgetLimit returns the per-agent daily token cap enforced on ctx, or 0
// when no agent dimension is set or the agent has no cap. The read side of
// WithAgentBudget's limit, symmetric with AgentBudgetID: it completes the
// used-versus-cap pair with AgentTokenUsageToday, so a caller can report an
// agent's remaining budget without re-reading the agent row.
func AgentBudgetLimit(ctx context.Context) int {
	for _, d := range budgetDimensions(ctx) {
		if d.Scope == BudgetScopeAgent {
			return d.Limit
		}
	}
	return 0
}

// ActorID returns the user UUID this call's AI spend is attributed to — the read
// side of WithActor, resolved identically to the meters (an explicit tag first,
// then the authenticated user on the request context). Empty when no user can be
// determined, in which case only the workspace and any extra dimensions apply.
func ActorID(ctx context.Context) string { return actorFromContext(ctx) }
