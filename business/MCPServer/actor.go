package business

// Who acted — the actor identity behind a call, as distinct from the credential it
// arrived with.
//
// WHY THIS IS NOT JUST THE TOKEN ID. Non-human identity guidance is blunt about it:
// a record showing only a token or a service account is too weak to establish intent,
// ownership or containment, and good auditability is identity plus context. During
// 2026 the identity vendors converged on agents as a first-class identity class —
// not a human to impersonate, not a shared service account to hide inside — with
// four parts: a distinct principal, scoped permissions, a clear owner, and an
// independent kill switch.
//
// OneCamp already had three of the four. ai_agents has an owner (created_by), scope
// (channel_ids, project_ids, enabled_tools) and a kill switch (is_active). What was
// missing was the binding from a credential to that identity, which migration 138
// adds as api_tokens.agent_id.
//
// The separation matters and is copied from how the vendors model it: an agent
// identity holds no credentials of its own. So an agent is a row, a token is a row,
// and one agent may hold several tokens — a rotation window, or one per client — all
// resolving to the same actor in the audit trail.
//
// ACTOR TYPE IS RECORDED, not inferred. A human at a keyboard, a plain integration
// script and an autonomous agent can all reach the same endpoint and leave very
// different risk signals. Writing the type down means "show me everything an agent
// did last week" is a query rather than a reconstruction.
//
// THE KILL SWITCH IS CHECKED PER CALL. Deactivating an agent must stop every
// credential bound to it on the NEXT REQUEST, without anyone hunting for tokens to
// revoke. A kill switch that waits for a rotation is not a kill switch. This is the
// same live-evaluation rule the resource check follows, for the same reason.

import (
	"context"
	"strings"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// ActorType is what KIND of thing made a call. Recorded so risk can be reasoned
// about by category, not by guessing from a name.
type ActorType string

const (
	// ActorAgent — a credential bound to an agent identity.
	ActorAgent ActorType = "agent"
	// ActorAPIClient — an unbound integration credential. A script, not an agent.
	// The honest label for every token that predates migration 138.
	ActorAPIClient ActorType = "api_client"
)

// ActorIdentity is the resolved answer to "who is calling".
type ActorIdentity struct {
	Type ActorType
	// TokenID is the credential. Present for both types.
	TokenID string
	// AgentID and AgentName are set only for ActorAgent, and are what an audit row
	// should name instead of a hex string.
	AgentID   string
	AgentName string
	// AgentDailyTokens is the agent's own daily AI token cap (0 = no per-agent cap),
	// carried out of the agent row this function already reads. Present so the caller
	// can enforce the SAME per-agent budget an in-app agent run gets, without a
	// second lookup — see SpendContext.
	AgentDailyTokens int
	// AgentEnabledTools is the agent's DECLARED TOOLSET, carried out of the same row.
	// An empty list means the agent has no tools at all (a conversation-only agent),
	// which is exactly how the in-app runner reads it — see CheckAgentToolScope.
	AgentEnabledTools []string
	// AgentScope is WHERE the agent may act: the channels and projects its owner
	// confined it to. Empty lists mean "wherever its owner can act", which is how every
	// in-app reader treats them — see CheckAgentResourceScope.
	AgentScope agentModel.AgentScope
	// PrincipalUserID is the human accountable for the credential.
	PrincipalUserID string
}

// Label is how this actor should read in an audit summary or an approval card.
// Written for a person: a named agent by name, an unbound credential as what it is.
func (a ActorIdentity) Label() string {
	if a.Type == ActorAgent && strings.TrimSpace(a.AgentName) != "" {
		return a.AgentName
	}
	if a.Type == ActorAgent {
		return "an agent"
	}
	return "an API client"
}

// ResolveActorForRequest resolves the actor behind a presented bearer credential in
// one step: validate the credential, then resolve the identity and apply the kill
// switch.
//
// FOR CALLERS THAT ONLY NEED TO KNOW WHO IS ASKING — the catalogue, which has to
// narrow what it shows to the agent's own toolset and must not advertise anything to a
// deactivated agent. The authorization ladders keep their rungs written out
// individually, because their ORDER is a property their tests assert directly and
// collapsing two rungs into one call would hide it.
//
// Returns ok=false for anything that is not a live, usable identity, so a caller can
// treat the false case as "show nothing, allow nothing" without interpreting a reason.
func ResolveActorForRequest(ctx context.Context, tokenPlaintext string) (ActorIdentity, bool, string) {
	auth, err := apiTokenBusiness.Validate(ctx, strings.TrimSpace(tokenPlaintext))
	if err != nil || auth == nil {
		return ActorIdentity{}, false, "invalid or inactive credential"
	}
	return ResolveActor(ctx, auth.TokenID.String(), auth.AgentID, auth.UserID.String())
}

// ResolveActor turns a validated credential into an actor identity, and enforces the
// agent kill switch.
//
// Returns ok=false when the credential is bound to an agent that is deactivated or
// deleted. That is a REFUSAL, not a downgrade to api_client: silently continuing as
// an unbound credential would mean deactivating an agent stopped its attribution
// while leaving its access intact, which is the opposite of a kill switch and the
// most dangerous possible reading of the flag.
//
// A lookup failure is also a refusal. We cannot establish that the agent is active,
// and proceeding on an unverified identity is exactly what the audit trail exists to
// make impossible.
func ResolveActor(ctx context.Context, tokenID string, agentID *uuid.UUID, principalUserID string) (actor ActorIdentity, ok bool, reason string) {
	base := ActorIdentity{
		Type:            ActorAPIClient,
		TokenID:         strings.TrimSpace(tokenID),
		PrincipalUserID: strings.TrimSpace(principalUserID),
	}

	// FAIL CLOSED, NEVER CRASH — the third place in this codebase needing this, so
	// it is a pattern rather than an accident: the database accessors dereference a
	// connection that is nil until the server has connected, so asking an identity
	// question during a blip panics instead of returning an error.
	//
	// For an identity question the only safe failure is a refusal, and for
	// availability the only safe failure is not taking the process down. An outage
	// should degrade to "no agent can act", never to "the server serving humans
	// dies".
	defer func() {
		if r := recover(); r != nil {
			actor, ok, reason = base, false, "the agent identity behind this credential could not be verified"
		}
	}()

	// Unbound credential. Legitimate and unchanged — every token predating migration
	// 138 is one, and a script is honestly an api_client rather than an agent.
	if agentID == nil {
		return base, true, "unbound integration credential"
	}

	agent, why := apiTokenBusiness.BoundAgent(ctx, *agentID)
	if agent == nil {
		return base, false, why
	}

	base.Type = ActorAgent
	base.AgentID = agent.Id.String()
	base.AgentName = agent.Name
	base.AgentDailyTokens = agent.MaxDailyTokens
	base.AgentEnabledTools = agent.EnabledToolList()
	base.AgentScope = agent.ScopeConfig()
	return base, true, "active agent identity"
}
