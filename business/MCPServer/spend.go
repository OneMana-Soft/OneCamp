package business

// What an MCP call's AI spend is billed to.
//
// THE GAP THIS CLOSES. Several governed tools call a model on the way to their
// answer — search_workspace embeds the query, summarize_channel and
// summarize_group_chat run a completion. Those calls were metered against the
// TOKEN OWNER'S personal daily quota (the AI layer falls back to the authenticated
// user on the request context) and against the workspace cap, which is correct as
// far as it goes. What never engaged was the PER-AGENT cap: nothing on this path
// tagged the agent dimension, so an admin could set an agent's max_daily_tokens,
// see it enforced on every in-app run, and have it silently ignored the moment the
// same agent worked through MCP. A cap that holds on one surface and not another is
// worse than no cap, because it is believed.
//
// WHY THE SAME RULE AS AN IN-APP RUN. An agent is one identity with one budget. The
// surface it happens to arrive on is a transport detail, and a budget that varies by
// transport is a budget an operator cannot reason about. So this reuses the agent
// runner's exact primitive (ai.WithAgentBudget) with the cap from the agent's own
// row, rather than inventing an MCP-specific tier.
//
// LAYERED, NOT SUBSTITUTED. The per-agent meter is added ON TOP of the user and
// workspace meters rather than replacing them, matching the agent runner. Every tier
// is independent and the strictest one wins, so binding a token to an agent can only
// ever tighten what that credential may spend — never loosen it. That direction is
// the point: it means this is safe to apply unconditionally.
//
// A 0 cap still meters. An agent with no configured cap is recorded for the usage
// breakdown and bounded by the workspace tier, so per-agent reporting is complete
// whether or not anyone chose a limit.

import (
	"context"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// SpendContext returns ctx tagged with the budget meters an MCP tool call's AI
// spend should be charged to.
//
// Generic and total: safe to call for every actor and every tool, including tools
// that never touch a model (tagging a context that nothing reads costs nothing).
// Applying it unconditionally is deliberate — the alternative, calling it only for
// tools believed to use AI, is a judgement that has to be re-made correctly every
// time a tool is added, and it is the kind of judgement that quietly rots.
//
// An unbound api_client actor is returned unchanged: there is no agent identity to
// bill, so the user and workspace tiers already in place are the whole answer.
func SpendContext(ctx context.Context, actor ActorIdentity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	// Attribute explicitly rather than relying on the AI layer's fallback to the
	// request's authenticated user. Same result today, but it makes the attribution
	// a stated property of this path instead of a coincidence of middleware order —
	// and it keeps working for any future caller that has no HTTP request.
	ctx = ai.WithActor(ctx, actor.PrincipalUserID)

	// Blank agent id is a documented no-op inside WithAgentBudget, so the api_client
	// case needs no branch here.
	return ai.WithAgentBudget(ctx, actor.AgentID, actor.AgentDailyTokens)
}
