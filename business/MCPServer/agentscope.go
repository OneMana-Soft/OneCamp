package business

// An agent's declared toolset, enforced on this surface too.
//
// THE GAP. The agent builder lets an owner choose exactly which tools an agent may
// use, and the in-app runner enforces that choice: it builds an allow-set from the
// agent's enabled_tools and every tool call is checked against it. The MCP surface
// consulted the list nowhere. So an agent restricted to "read docs and summarise" in
// the builder could, through a credential bound to it, reach every tool its owner's
// token scope allowed — create tasks, send messages, write table rows.
//
// This is the same failure as the per-agent token cap: a restriction an owner sets,
// honoured on one surface and silently ignored on another. The owner has no way to
// discover it, because the builder shows the restriction as applied.
//
// SCOPED PERMISSIONS ARE ONE OF THE FOUR LEGS the agent-identity model rests on — a
// distinct principal, scoped permissions, a clear owner, an independent kill switch.
// Migration 138 added the credential binding and noted OneCamp already had the other
// three. It had the SCOPE, in the sense that it was recorded and enforced in-app; what
// it did not have was that scope holding wherever the identity acts.
//
// AN EMPTY LIST MEANS NO TOOLS, not "all tools". That is what the runner already does
// — agentHasTools is len(list) > 0, and a conversation-only agent is a real and common
// configuration — so reading it as "unrestricted" here would invert the owner's
// intent in the one case where the intent is most explicit. It is also the safe
// direction for a malformed column, which parses to an empty list.
//
// This costs nothing to check: the list is already in hand from the row ResolveActor
// reads for the kill switch, and the comparison is a string match over a handful of
// entries. So it belongs immediately after identity resolution, before anything
// expensive.

import "strings"

// CheckAgentToolScope decides whether this actor's agent identity is permitted to use
// the named tool.
//
// Total and safe for every actor: an unbound api_client has no agent scope to apply
// and is always allowed through, so callers apply it unconditionally rather than
// branching on actor type — the same reason SpendContext is applied unconditionally.
func CheckAgentToolScope(actor ActorIdentity, toolName string) (bool, string) {
	if actor.Type != ActorAgent {
		return true, "unbound credential: no agent toolset to apply"
	}

	tool := strings.TrimSpace(toolName)
	if tool == "" {
		return false, "no tool named"
	}

	if len(actor.AgentEnabledTools) == 0 {
		// Named rather than described, because the owner's next step is to open the
		// builder and grant something.
		return false, actor.Label() + " has no tools enabled, so it cannot act on the workspace"
	}

	for _, t := range actor.AgentEnabledTools {
		if t == tool {
			return true, "tool is in " + actor.Label() + "'s enabled toolset"
		}
	}

	// Deliberately does NOT list what the agent CAN do. A refusal that enumerates the
	// rest of the toolset hands a caller a map of an identity's capabilities, and the
	// legitimate reader of this message already has the builder open.
	return false, tool + " is not in " + actor.Label() + "'s enabled toolset"
}

// CheckAgentResourceScope decides whether this actor's agent identity may act on the
// object a call has resolved.
//
// THE SECOND HALF OF THE AGENT'S SCOPE. CheckAgentToolScope answers WHAT an agent may
// use; this answers WHERE it may use it. An owner who confines an agent to #support means
// it for wherever that agent acts, and until now the confinement held for in-app runs and
// not for a credential bound to the same agent.
//
// IT MATCHES outsideScope IN business/AIAgent EXACTLY, including the part that surprises
// people: a call that names NEITHER a channel nor a project is never out of scope. An
// agent confined to #support can still read a document or run a workspace search, in-app
// and here alike, because the confinement is about the channels and projects it acts in
// rather than a general reduction of its owner's visibility. Inventing the stricter rule
// only on this surface would be the same mistake in the opposite direction — a control
// that behaves differently depending on how the agent was invoked.
//
// So the mapping is deliberately narrow:
//
//	ResourceChannel -> checked against the channel scope
//	ResourceProject -> checked against the project scope
//	everything else -> unconstrained, matching in-app
//
// A TASK IS NOT CHECKED AGAINST THE PROJECT SCOPE, and that is the shipped behaviour
// rather than an oversight: the in-app rule reads the call's project_uuid argument, and a
// task write names only the task. Adding the lookup here would make MCP stricter than the
// app for the same agent, which is exactly the divergence this work keeps closing. If
// that confinement is wanted it should be added to both, deliberately, as one change.
//
// Total and safe for every actor: an unbound api_client has no agent scope, and an agent
// with empty lists is unrestricted, so callers apply it unconditionally.
func CheckAgentResourceScope(actor ActorIdentity, ref ResourceRef) (bool, string) {
	if actor.Type != ActorAgent {
		return true, "unbound credential: no agent scope to apply"
	}

	id := strings.TrimSpace(ref.ID)

	switch ref.Kind {
	case ResourceChannel:
		if len(actor.AgentScope.ChannelIDs) == 0 || id == "" {
			return true, "no channel confinement"
		}
		if containsID(actor.AgentScope.ChannelIDs, id) {
			return true, actor.Label() + " is scoped to this channel"
		}
		// Does NOT name the channels the agent IS scoped to. A refusal that listed them
		// would hand a caller a map of where an identity operates.
		return false, actor.Label() + " is not scoped to this channel"

	case ResourceProject:
		if len(actor.AgentScope.ProjectIDs) == 0 || id == "" {
			return true, "no project confinement"
		}
		if containsID(actor.AgentScope.ProjectIDs, id) {
			return true, actor.Label() + " is scoped to this project"
		}
		return false, actor.Label() + " is not scoped to this project"
	}

	return true, "this resource kind is not confined by agent scope"
}

// containsID compares trimmed ids, because a scope list is admin-entered JSON and a
// stray space must not silently exclude a channel someone believes they granted.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if strings.TrimSpace(id) == want {
			return true
		}
	}
	return false
}

// CheckAgentArgScope applies the same confinement to a call whose resource has NOT been
// resolved, by reading the channel and project out of its arguments.
//
// FOR THE UNGOVERNED PATH, which has no ResourceRef because no authority rule has been
// written for those tools yet. It would have been easy to leave the confinement off there
// and call it a consequence of not being governed — but the argument names are exactly
// what the in-app rule reads (outsideScope inspects params["channel_uuid"] and
// params["project_uuid"]), so the same rule is available without a resource resolver.
// Skipping it would have meant an owner's confinement holding for 22 tools and not the
// other 8, which is the uneven guarantee this work exists to remove.
//
// Delegates to CheckAgentResourceScope rather than repeating the comparison, so there is
// one rule reached two ways: from a resolved resource when there is one, and from the
// arguments when there is not. Both are checked independently, matching in-app — a call
// naming both a channel and a project must satisfy both confinements.
func CheckAgentArgScope(actor ActorIdentity, args map[string]any) (bool, string) {
	if actor.Type != ActorAgent {
		return true, "unbound credential: no agent scope to apply"
	}

	for _, probe := range []struct {
		arg  string
		kind ResourceKind
	}{
		{"channel_uuid", ResourceChannel},
		{"project_uuid", ResourceProject},
	} {
		id := strings.TrimSpace(asString(args[probe.arg]))
		if id == "" {
			continue
		}
		if allowed, reason := CheckAgentResourceScope(actor, ResourceRef{Kind: probe.kind, ID: id}); !allowed {
			return false, reason
		}
	}

	return true, "within " + actor.Label() + "'s scope"
}
