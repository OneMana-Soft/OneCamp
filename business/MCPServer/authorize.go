package business

// AuthorizeToolCall — the seam every MCP tool call passes through.
//
// This is the product. Everything above it (JSON-RPC framing, HTTP) is
// replaceable transport; everything below it is existing business logic. If MCP is
// superseded, a new transport calls this same function.
//
// ORDER OF THE LADDER IS LOAD-BEARING. Checks run cheapest-and-most-absolute
// first, so a refusal never depends on a lookup that could itself fail, and a
// caller with no valid token never causes a database query:
//
//	1. actor      is the token real, active, unexpired?      (1 indexed read)
//	2. tool       does it exist and is it enabled?           (map lookup)
//	3. scope      does the token carry the tool's scope?      (pure)
//	4. resource   can the tool resolve what it will touch?    (pure)
//	5. capability does the PRINCIPAL hold the capability?     (cached policy)
//	6. reach      is the PRINCIPAL permitted on that object?  (graph reads)
//
// Steps 2–4 are free. Step 6 is the expensive one and runs last, only for a call
// that would otherwise succeed.
//
// EVERY outcome is audited by the caller, allow and deny alike. Refusals are the
// valuable half of the record: "your agent tried to read #board-private and was
// stopped because the person who authorised it is not a member" is the sentence an
// auditor asks for, and the sentence a gateway cannot produce.

import (
	"context"
	"strings"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	authz "github.com/akashc777/OneCamp/business/Authz"
	aiSettingsModel "github.com/akashc777/OneCamp/models/postgres/AI"
)

// ToolCallRequest is one inbound call, before any identity is resolved.
type ToolCallRequest struct {
	// TokenPlaintext is the presented bearer credential. Never logged, never
	// audited; only its id and prefix are recorded.
	TokenPlaintext string
	// ToolName is the requested tool.
	ToolName string
	// Args are the raw arguments from the client.
	Args map[string]any
	// ClientName / ClientVersion identify the calling software. Under the stateless
	// 2026-07-28 revision these arrive in per-request `_meta` rather than from a
	// handshake, since `initialize` no longer exists. Recorded so the audit trail
	// says which agent software acted, not merely that something did.
	ClientName    string
	ClientVersion string
	// Limits are the per-credential call budgets to enforce. Zero values mean the
	// defaults, so a caller may leave this empty and get the safe behaviour.
	Limits BudgetLimits
}

// Decision is the authorization outcome. Reason is always populated so the audit
// record and the client-facing message come from one place.
type Decision struct {
	Allow  bool
	Reason string
	// Spec is the resolved tool, non-nil only on allow.
	Spec *ToolSpec
	// Call is the context handed to the handler, valid only on allow.
	Call ToolCallContext
	// TokenID and PrincipalUserID are populated as soon as they are known, INCLUDING
	// on refusal, so a denial can still be attributed in the audit trail. A refusal
	// nobody can attribute is a refusal nobody can investigate.
	TokenID         string
	PrincipalUserID string
	// Actor is the resolved identity — a named agent, or an unbound API client.
	// Populated for every outcome once the credential is valid, because "which agent
	// was refused" is as much a part of the record as "which agent succeeded".
	Actor ActorIdentity
	// RetryAfter is set on a budget refusal so a well-behaved client knows when to
	// come back rather than hammering. Zero for every other outcome — a permission
	// refusal is not something waiting will fix, and suggesting otherwise would
	// invite exactly the retry loop the budget exists to stop.
	RetryAfter time.Duration
}

// deny is a small helper so every refusal is shaped identically and carries
// whatever identity was resolved before the refusal happened.
func deny(reason, tokenID, principal string) Decision {
	return Decision{Allow: false, Reason: reason, TokenID: tokenID, PrincipalUserID: principal}
}

// denyAs is deny for refusals that happen AFTER the actor is known, so the record
// names which agent was refused rather than only which credential. "Which agent was
// stopped" is as much a part of the evidence as which one succeeded.
func denyAs(actor ActorIdentity, reason, tokenID, principal string) Decision {
	d := deny(reason, tokenID, principal)
	d.Actor = actor
	return d
}

// AuthorizeToolCall decides whether a call may proceed.
//
// Does no I/O beyond the token lookup, the capability policy (cached) and the
// permission graph reads inside PrincipalCanReach. Performs no mutation and has no
// side effects, so it is safe to call speculatively and safe to test.
func AuthorizeToolCall(ctx context.Context, req ToolCallRequest) Decision {
	// 1. ACTOR. Validate returns (nil, nil) for a missing or inactive token so every
	// failure answers identically — a distinguishable "expired" versus "unknown"
	// reply is a probing oracle.
	auth, err := apiTokenBusiness.Validate(ctx, req.TokenPlaintext)
	if err != nil || auth == nil {
		return deny("invalid or inactive credential", "", "")
	}
	tokenID := auth.TokenID.String()
	principal := auth.UserID.String()

	// 1b. ACTOR IDENTITY, and the agent kill switch. Immediately after the
	// credential, because a deactivated agent must stop on the NEXT CALL rather
	// than at the next token rotation — and because everything after this point
	// should be attributable to a named actor, including the refusals.
	actor, ok, actorReason := ResolveActor(ctx, tokenID, auth.AgentID, principal)
	if !ok {
		d := deny(actorReason, tokenID, principal)
		d.Actor = actor
		return d
	}

	// 1c. THE AGENT'S OWN DECLARED TOOLSET. Immediately after the identity, because it
	// is free (the list came out of the row just read), absolute (an owner's explicit
	// restriction cannot be widened by anything below), and it is the agent's scope —
	// the leg of the identity model that was recorded and enforced in-app but not here.
	// See agentscope.go.
	if allowed, scopeReason := CheckAgentToolScope(actor, req.ToolName); !allowed {
		return denyAs(actor, scopeReason, tokenID, principal)
	}

	// 2. TOOL. An unknown name is refused without saying what would have been valid,
	// so a client cannot enumerate a catalogue it was not granted.
	spec, ok := Lookup(req.ToolName)
	if !ok {
		return denyAs(actor, "unknown tool", tokenID, principal)
	}

	// 2b. ADMISSION. What the WORKSPACE has agreed to expose, which is a different
	// question from what this person may do — and the admin's decision rather than the
	// caller's.
	//
	// Placed before the scope check because it is the broader gate: if the surface is
	// off, or this tool's group was never enabled, nothing about the credential can
	// change the answer. Refusing here also means a disabled workspace spends nothing on
	// permission lookups.
	//
	// Loaded HERE rather than accepted from the caller, for the same reason the token is
	// re-validated rather than trusted: a caller that could supply the settings could
	// supply "enabled", and the caller is the layer whose mistakes this function exists
	// to survive. One singleton read is a cheap price for that.
	//
	// A settings read failure is a refusal. The admin's decision is unknown, and the
	// only safe reading of an unknown admission decision is that the surface is closed.
	settings, serr := aiSettingsModel.GetSettings(ctx)
	if serr != nil || settings == nil {
		return denyAs(actor, "the workspace's MCP settings could not be read; refusing", tokenID, principal)
	}
	if adm := CheckAdmission(AdmissionSettings{
		Enabled:    settings.MCPEnabled,
		ToolGroups: settings.MCPToolGroups,
	}, spec); !adm.Allow {
		return denyAs(actor, adm.Reason, tokenID, principal)
	}

	// 3. SCOPE. Pure, and it bounds everything after it.
	if !hasScope(auth.Scopes, spec.RequiredScope) {
		return denyAs(actor, "this credential does not carry the scope this tool requires", tokenID, principal)
	}

	// 4. RESOURCE RESOLUTION. Done before any permission work, because a tool that
	// cannot say what it will touch cannot be authorised at all. A resolution error
	// is a refusal and never a fallback to workspace scope — falling back would turn
	// a malformed argument into a privilege escalation.
	ref, rerr := spec.Resource(req.Args)
	if rerr != nil {
		return denyAs(actor, "could not determine what this call would act on: "+rerr.Error(), tokenID, principal)
	}
	if ref.Kind == "" {
		return denyAs(actor, "tool resolved an empty resource kind", tokenID, principal)
	}
	// ACCESS IS DERIVED HERE, and whatever the Resource function put there is
	// discarded. A tool declares what it DOES via Behaviour; the access it needs
	// follows from that and is not a second, independently editable claim.
	//
	// Overwriting rather than validating is deliberate. If a Resource function could
	// set this, then the one field able to understate a call's intent would be set by
	// the same author whose tool would benefit from understating it. Derived from
	// ReadOnly, the field cannot disagree with the behaviour the registry already
	// enforces and advertises.
	ref.Access = AccessWrite
	if spec.Behaviour.ReadOnly {
		ref.Access = AccessRead
	}

	// 4b. THE AGENT'S OWN CONFINEMENT — WHERE it may act, now that the object is known.
	//
	// Placed here because it needs the resolved resource and nothing else: it is pure,
	// free, and absolute, so it runs before the capability lookup and the graph reads
	// rather than after them. An owner's explicit confinement cannot be widened by
	// anything below it.
	//
	// The companion to the toolset check at rung 1c: that one asked WHAT, this asks
	// WHERE. See agentscope.go for why the mapping is narrow and matches the in-app rule
	// exactly rather than being stricter here.
	if allowed, scopeReason := CheckAgentResourceScope(actor, ref); !allowed {
		return denyAs(actor, scopeReason, tokenID, principal)
	}

	// 5. CAPABILITY, against the PRINCIPAL rather than the token. A token cannot
	// hold a capability its human does not.
	if cap := strings.TrimSpace(spec.RequiredCapability); cap != "" {
		// Reuses the existing loader rather than adding a second one, for the same
		// reason PrincipalCanReach reuses the existing membership lookups.
		userInfo, uerr := aiBusiness.BuildUserInfoByUserUUID(ctx, principal)
		if uerr != nil || userInfo == nil {
			return denyAs(actor, "the originating person could not be resolved for a capability check", tokenID, principal)
		}
		if !authz.Can(ctx, userInfo, cap) {
			return denyAs(actor, "the originating person does not hold the capability this tool requires", tokenID, principal)
		}
	}

	// 6. REACH. The expensive check, and the one that makes this different from a
	// gateway: the principal's LIVE permission on the specific object.
	reach := PrincipalCanReach(ctx, principal, ref)
	if !reach.Allow {
		return denyAs(actor, reach.Reason, tokenID, principal)
	}

	// 7. BUDGET, last, because it CONSUMES. Charging a call that was going to be
	// refused for lacking a scope would let an unauthorised caller drain a
	// legitimate one's budget — a denial of service disguised as a rate limit. So
	// every free check runs first and only a call that would otherwise succeed
	// spends anything.
	if b := CheckBudget(ctx, tokenID, spec, req.Limits); !b.Allow {
		d := denyAs(actor, b.Reason, tokenID, principal)
		d.RetryAfter = b.RetryAfter
		return d
	}

	return Decision{
		Allow:           true,
		Reason:          "authorised: " + reach.Reason,
		Actor:           actor,
		Spec:            spec,
		TokenID:         tokenID,
		PrincipalUserID: principal,
		Call: ToolCallContext{
			PrincipalUserID:    principal,
			PrincipalDgraphUID: reach.PrincipalDgraphUID,
			TokenID:            tokenID,
			Resource:           ref,
			Args:               req.Args,
		},
	}
}
