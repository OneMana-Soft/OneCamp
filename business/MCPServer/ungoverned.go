package business

// The surface gate for tools that are NOT governed yet.
//
// WHAT WAS WRONG. Governance is being added tool by tool, because writing down what
// authority over an object means is genuinely per-tool work. Half the public catalogue
// has it; half does not. The half that does not was falling through to a path that
// checked ONE thing — does the token carry the scope — and then ran the executor.
//
// That is defensible for object authority: no rule has been written, so the executor's
// own permission scoping is what there is. It was NOT defensible for everything else,
// and four workspace-level controls were being skipped by tools an admin had every
// reason to believe were covered:
//
//	admission     an admin could switch the MCP surface OFF, or enable only the
//	              tasks group, and these tools kept serving. The control was
//	              advertised and did not hold — the worst kind of control.
//	kill switch   deactivating an agent is documented as taking effect on the NEXT
//	              call. For these tools it took effect never.
//	call budget   no per-credential rate cap, so the runaway-loop failure mode the
//	              budget exists for was unbounded here.
//	spend         the per-agent daily token cap could not engage, because the
//	              executor ran on a context with no actor attribution.
//
// None of those four need a per-tool authority rule. They ask about the CREDENTIAL,
// the IDENTITY, and the WORKSPACE — questions that are fully answerable for any tool
// with a name and a scope. So the frontier between governed and ungoverned should
// never have run through them, and now it does not.
//
// WHY THIS IS A SEPARATE FUNCTION FROM AuthorizeToolCall, and not a shared prefix.
// The two paths give genuinely different guarantees: that one ends with the
// principal's live permission on a named object, and this one cannot. Giving them one
// name would imply an equivalence that does not exist, and the next person to add a
// tool would have no way to see which half they had landed in. Every RUNG is still a
// single shared function — nothing is reimplemented here, only composed — and
// TestBothLaddersRunTheSameSurfaceRungsInTheSameOrder stops the two orders drifting.

import (
	"context"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	aiSettingsModel "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// UngovernedCallRequest is one inbound call to a tool with no authority rule yet.
type UngovernedCallRequest struct {
	// TokenPlaintext is the presented bearer credential. Never logged or audited.
	TokenPlaintext string
	// ToolName is the requested tool. Its scope and read/write behaviour are resolved
	// from the registries here rather than accepted from the caller, so a caller
	// cannot understate a write as a read to reach the more generous budget.
	ToolName string
	// Args are the raw arguments from the client. Read ONLY to apply an agent's
	// channel/project confinement, which is the same thing the in-app rule reads them
	// for. No authority is derived from them: an argument a caller controls cannot
	// establish permission, only narrow it.
	Args map[string]any
	// Limits are the per-credential call budgets. Zero values mean the defaults.
	Limits BudgetLimits
}

// UngovernedDecision is the outcome. Shaped like Decision, and deliberately NOT the
// same type: this one carries no resolved resource, because establishing one is
// exactly what has not been done for these tools. A caller cannot mistake it for a
// full authorization.
type UngovernedDecision struct {
	Allow  bool
	Reason string
	// TokenID and PrincipalUserID are populated as soon as known, including on
	// refusal, so a denial is still attributable.
	TokenID         string
	PrincipalUserID string
	// Actor is the resolved identity, populated for every outcome once the credential
	// is valid.
	Actor ActorIdentity
	// RetryAfter is set only on a budget refusal, so a client knows waiting will help.
	// Zero elsewhere: waiting does not fix a permission or admission refusal, and
	// implying otherwise invites the retry loop the budget exists to stop.
	RetryAfter time.Duration
}

// AuthorizeUngovernedCall runs every surface-level gate that does not require a
// per-tool authority rule, in the same order as the governed ladder.
//
// Cheapest-and-most-absolute first, so a refusal never depends on a lookup that could
// itself fail and an invalid credential causes no further work:
//
//  1. actor      is the token real, active, unexpired?     (1 indexed read)
//  2. identity   is the agent behind it still alive?       (1 indexed read)
//  3. admission  does the workspace expose this group?     (1 settings read)
//  4. scope      does the token carry the tool's scope?    (pure)
//  5. budget     LAST, because it consumes.
//
// The caller must audit every outcome, allow and deny alike.
func AuthorizeUngovernedCall(ctx context.Context, req UngovernedCallRequest) UngovernedDecision {
	// 1. ACTOR. Identical failure text for missing, unknown and expired credentials:
	// a distinguishable reply is a probing oracle.
	auth, err := apiTokenBusiness.Validate(ctx, req.TokenPlaintext)
	if err != nil || auth == nil {
		return UngovernedDecision{Reason: "invalid or inactive credential"}
	}
	tokenID := auth.TokenID.String()
	principal := auth.UserID.String()
	base := UngovernedDecision{TokenID: tokenID, PrincipalUserID: principal}

	// 2. ACTOR IDENTITY, and the agent kill switch. Immediately after the credential,
	// because a deactivated agent must stop on the NEXT CALL rather than whenever
	// someone remembers to hunt down its tokens.
	actor, ok, actorReason := ResolveActor(ctx, tokenID, auth.AgentID, principal)
	base.Actor = actor
	if !ok {
		base.Reason = actorReason
		return base
	}

	// 2b. THE AGENT'S OWN DECLARED TOOLSET. Free, absolute, and the agent's own
	// scope — an owner's explicit restriction in the builder must hold here exactly as
	// it holds for an in-app run. See agentscope.go.
	if allowed, scopeReason := CheckAgentToolScope(actor, req.ToolName); !allowed {
		base.Reason = scopeReason
		return base
	}

	// 2c. AND WHERE IT MAY ACT. No resource has been resolved for these tools, so the
	// confinement is read from the call's own channel_uuid / project_uuid arguments —
	// which is precisely what the in-app rule reads. Leaving it off here would mean an
	// owner's confinement covering the governed tools and not these.
	if allowed, scopeReason := CheckAgentArgScope(actor, req.Args); !allowed {
		base.Reason = scopeReason
		return base
	}

	// The scope is what places a tool in an admin-approved group, so an unlisted tool
	// is refused before the settings read. Not publicly exposable is not an error
	// state — it is most of the tool registry, which is internal by default.
	scope, public := apiTokenBusiness.ScopeForTool(req.ToolName)
	if !public {
		base.Reason = "unknown tool"
		return base
	}

	// 3. ADMISSION. What the WORKSPACE has agreed to expose, which is the admin's
	// decision rather than the caller's, and the broader gate: if the surface is off,
	// nothing about the credential can change the answer.
	//
	// A settings read failure is a REFUSAL. The admin's decision is unknown, and the
	// only safe reading of an unknown admission decision is that the surface is shut.
	settings, serr := aiSettingsModel.GetSettings(ctx)
	if serr != nil || settings == nil {
		base.Reason = "the workspace's MCP settings could not be read; refusing"
		return base
	}
	if adm := CheckAdmissionForScope(AdmissionSettings{
		Enabled:    settings.MCPEnabled,
		ToolGroups: settings.MCPToolGroups,
	}, scope); !adm.Allow {
		base.Reason = adm.Reason
		return base
	}

	// 4. SCOPE. Pure, and read from the token row rather than from the request
	// context, so this function's answer does not depend on middleware having run.
	if !hasScope(auth.Scopes, scope) {
		base.Reason = "this credential does not carry the scope this tool requires"
		return base
	}

	// 5. BUDGET, last, because it CONSUMES. Charging a call that was going to be
	// refused would let an unauthorised caller drain a legitimate credential's
	// budget — a denial of service disguised as a rate limit.
	//
	// Read/write comes from the AI tool registry, never from the caller: the registry
	// is what actually decides whether the executor mutates anything.
	if b := CheckCallBudget(ctx, tokenID, ai.ToolIsReadOnly(req.ToolName), req.Limits); !b.Allow {
		base.Reason = b.Reason
		base.RetryAfter = b.RetryAfter
		return base
	}

	base.Allow = true
	base.Reason = "admitted: " + actor.Label() + " holds " + scope
	return base
}
