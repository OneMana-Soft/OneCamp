package business

// Governed MCP server — the tool contract.
//
// MCP is how external agents will reach a company's internal systems: SDK
// downloads went from roughly 100K to ~97M a month between late 2024 and early
// 2026, faster than Kubernetes reached comparable adoption. It is also, by
// published audit, badly secured — around 40% of public MCP servers require no
// authentication at all, ~79% keep credentials in plaintext, and 40+ CVEs landed
// against MCP implementations during 2026. The protocol itself specifies no
// authentication or authorization; every server does it separately, and most do
// not do it.
//
// The named structural weakness is the interesting one: an MCP server sits between
// a client, a model and downstream systems, which reintroduces the CONFUSED DEPUTY
// problem inside a new protocol. That is the same failure OneCamp already refuses
// internally, where a delegated agent may never reach a surface the originating
// person could not address themselves.
//
// This file defines the contract that makes those refusals structural rather than
// remembered. A tool DECLARES what it touches; it never decides its own
// authorization. One function (see reach.go) decides for all of them, so adding a
// tool cannot add a new way to get authorization wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ResourceKind enumerates the things a tool may touch. Closed on purpose: an
// unknown kind is a refusal (see PrincipalCanReach), so a tool cannot smuggle in a
// resource type nobody has written an authority rule for.
type ResourceKind string

const (
	// ResourceChannel — authority is channel membership.
	ResourceChannel ResourceKind = "channel"
	// ResourceTask — authority is membership of the project owning the task.
	ResourceTask ResourceKind = "task"
	// ResourceDoc — authority is the doc's own reader/editor/commenter lists, the
	// doc being non-private, or having created it.
	ResourceDoc ResourceKind = "doc"
	// ResourceChat — a DM or group-chat GROUPING. Authority is participation in it.
	//
	// One kind for both, because a DM and a group chat are the same object with a
	// different number of participants; splitting them would produce two rules that
	// have to be kept saying the same thing.
	//
	// The ID is the grouping id. A tool that instead names the OTHER PERSON (as
	// summarize_dm does) is not this kind: the conversation it means is defined by
	// the caller plus that person, so the caller is a participant by construction
	// and there is no object they could name that is not theirs.
	ResourceChat ResourceKind = "chat"
	// ResourceTable — authority is the table's own visibility rule: admin, its
	// creator, or the table being workspace-visible. Delegated to
	// dataTableBusiness.CanView, which is the same check the app and the in-app
	// tools use.
	//
	// Note the actor for this kind comes from POSTGRES, not from the graph: tables
	// are a Postgres entity and their rule tests an admin flag that Dgraph's copy
	// of does not authoritatively hold.
	ResourceTable ResourceKind = "table"
	// ResourceProject — authority is project membership. The same rule the in-app
	// read_project executor applies, via the same lookup, so the two cannot drift.
	//
	// Membership grants SIGHT of a project. It does not grant the right to change
	// it or to create work inside it: that is project ADMIN, a distinction this
	// kind does not express, which is why the task-writing tools are not governed
	// by it.
	ResourceProject ResourceKind = "project"
	// ResourceDirectMessage — the one-to-one conversation between the caller and one
	// other PERSON, and the ID is that person's user uuid rather than a grouping id.
	//
	// WHY THE ID IS A PERSON. A DM in OneCamp has no identifier of its own: its
	// grouping id is derived from the two participants (helpers.GetGroupingId), so
	// naming the other person names the conversation exactly, and there is no
	// conversation the caller could name that is not theirs. That is what makes the
	// caller's own side of the exchange a non-question and leaves only the recipient
	// to decide about.
	//
	// SEPARATE FROM ResourceChat, which is a grouping the caller must be shown to
	// participate in. Here participation is structural, and the authority question is
	// the OTHER one: may this identity be written to at all. A deactivated account or
	// an attribution-only ghost cannot read anything sent to it, and the shipped DM
	// controller has always refused both — that is the rule this kind carries, via
	// business/Principal.CanReceiveDirectMessage.
	//
	// Bots ARE valid targets. The shared automation bot is is_external by class and
	// intentionally messageable, because a DM to it is answered by the AI coworker.
	// A kind that refused it would break a shipped feature.
	// ResourceDataSource — an admin-registered connection to an EXTERNAL database.
	// Authority is the source's own query rule, delegated to
	// dataSourceBusiness.CanQuery: an admin, its creator, or the source being
	// workspace-visible, and the source being enabled.
	//
	// THE MOST CONSEQUENTIAL KIND ON THIS SURFACE, because a mistake here does not
	// expose a channel — it exposes a customer's warehouse. That is why it waited for a
	// rule written on purpose rather than being inferred from a rule for something else.
	//
	// The pending note for these tools used to say authority was the agent.manage
	// capability. That was wrong, and worth recording as wrong: agent.manage gates the
	// CONFIG routes at the router — registering, editing, deleting a connection. Using
	// it for a query would have been a materially different and stricter rule than the
	// one the app applies, which is a per-source visibility check.
	//
	// Like ResourceTable, the actor comes from POSTGRES: the rule tests an admin flag
	// whose authoritative copy lives there, not in the graph.
	//
	// READ ONLY. Nothing on this surface writes to an external source — the connectors
	// are read-only by construction and no MCP tool offers a write — so the write branch
	// refuses rather than inventing an authority for something that cannot happen.
	// ResourceTeam — membership to read, team ADMIN to write. The same split
	// ResourceProject makes and for the same reason: being in a team lets you see it,
	// and creating a project inside it is an administrative act.
	//
	// EXISTS FOR THE CONTAINER OF A CREATION. A create names no object of its own, so it
	// must be authorised against the thing it creates INTO. For a project that container
	// is a team, and team admin is exactly what the shipped create-project path already
	// requires — so this expresses a rule the product had rather than inventing one.
	ResourceTeam          ResourceKind = "team"
	ResourceDataSource    ResourceKind = "data_source"
	ResourceDirectMessage ResourceKind = "direct_message"
	// ResourceWorkspace — authority is workspace-level only, for tools that touch
	// no single object (e.g. "list the channels I can see"). Such tools MUST scope
	// their own results to the principal; the authorizer cannot do it for them.
	ResourceWorkspace ResourceKind = "workspace"
	// ResourceSelfOwned — a tool that CREATES something belonging to the caller and
	// sitting in no container: a document they own, a reminder for themselves.
	//
	// THIS KIND CARRIES NO PER-OBJECT AUTHORITY, and it is named and documented that way
	// on purpose rather than being quietly folded into another kind. There is no object
	// to check: it does not exist yet, and when it does it will belong to the person who
	// asked. The authority is that person being a live, eligible member — established at
	// the top of the ladder — plus the token scope.
	//
	// WHY IT EXISTS AT ALL, given it adds no object check. Without it these tools sit on
	// the ungoverned path and skip nothing less than the write machinery: no idempotency
	// key, so a retried create_doc makes two documents. Bringing them here gives them
	// deduplication, the approval path if they ever become destructive, and a resource
	// kind in the audit row — everything the governed path offers except the one thing
	// that cannot exist for them.
	//
	// NOT ResourceWorkspace, which refuses writes outright and should keep doing so: a
	// tool that CHANGES an existing thing must name it. The distinction is real —
	// "creates something new that will be mine" is not "acts on the workspace at large" —
	// and collapsing them would either weaken the workspace rule for every tool or
	// leave these two ungoverned forever.
	//
	// DO NOT USE for anything that touches an object that already exists, or anything
	// whose result other people own. If a tool creates INTO a container, the container
	// is what must permit it — see ResourceTeam and ResourceProject.
	ResourceSelfOwned ResourceKind = "self_owned"
)

// IdentifiesNoObject reports whether this kind names a single object at all.
//
// Two kinds do not, for opposite reasons: ResourceWorkspace acts across many things, and
// ResourceSelfOwned acts on something that does not exist yet. Both consequences are the
// same wherever the code asks "which object is this" — the resource resolver needs no id
// argument, and there is no liveness to check — so the question is asked once here rather
// than as a growing list of special cases.
func (k ResourceKind) IdentifiesNoObject() bool {
	return k == ResourceWorkspace || k == ResourceSelfOwned
}

// Access is WHAT a call intends to do to its target: look at it, or change it.
//
// A separate dimension from ResourceKind, because the two are independent. The kind
// names the object; the access names the verb. Folding the verb into the kind would
// mean a ResourceProjectAdmin beside ResourceProject and the list would double every
// time a new verb appears, while a reviewer lost the ability to see at a glance which
// objects exist.
//
// NOT DECLARED BY TOOL AUTHORS. It is derived from the tool's Behaviour by
// AuthorizeToolCall — a read-only tool asks for read, anything else asks for write.
// Derivation rather than declaration is the point: a tool cannot claim read access
// while performing a write, because the same field that would be lying is the one
// that decides.
type Access string

const (
	// AccessRead is "may this person see this object".
	AccessRead Access = "read"
	// AccessWrite is "may this person change this object". A strictly stronger
	// claim: every kind here has objects a person may read and may not modify.
	AccessWrite Access = "write"
)

// IsRead reports whether this is a read request.
//
// Deliberately phrased so that ONLY the exact value "read" counts as one. The zero
// value is therefore a write, which is the safe direction: a code path that forgets
// to set Access gets the stricter check rather than the weaker one, and a typo in a
// literal fails closed instead of quietly granting read access to a mutation.
func (a Access) IsRead() bool { return a == AccessRead }

// ResourceRef identifies the specific object a call will act on, and what it means
// to do to it. Resolved from the tool's arguments BEFORE the handler runs, so
// authorization happens on the real target rather than on the tool's name.
type ResourceRef struct {
	Kind ResourceKind
	// ID is the object uuid. Empty is valid only for ResourceWorkspace.
	ID string
	// Access is set by the authorizer from the tool's declared Behaviour, NOT by the
	// tool's Resource function. A Resource function that sets it is overruled, and a
	// test enforces that it is derived rather than trusted.
	Access Access
}

// ToolCallContext is what a handler receives. It carries the resolved identities so
// a handler never re-derives them (and so cannot derive them differently).
type ToolCallContext struct {
	// PrincipalUserID is the HUMAN who authorised this agent — the api_token's
	// created_by. Every handler must scope its work to this person, never to the
	// token and never to an admin.
	PrincipalUserID string
	// PrincipalDgraphUID is the same person's graph node, already resolved.
	PrincipalDgraphUID string
	// TokenID identifies the actor. For audit only; never an authority.
	TokenID string
	// Resource is the authorised target.
	Resource ResourceRef
	// Args are the validated tool arguments.
	Args map[string]any
}

// ToolSpec is the static, server-controlled declaration of one tool.
//
// Description and InputSchema MUST be compile-time constants. Tool catalogues are a
// documented prompt-injection vector: names, descriptions and schemas enter a
// model's context as trusted instructions, so any user-controlled text reaching
// them is an injection channel into somebody else's agent. Registration enforces
// that a description is non-empty and the registry is the only way to publish a
// tool.
type ToolSpec struct {
	// Name is the MCP tool name. Stable; agents will hardcode it.
	Name string
	// Description is shown to the model. STATIC TEXT ONLY.
	Description string
	// InputSchema is the JSON Schema for Args. STATIC TEXT ONLY.
	InputSchema json.RawMessage
	// RequiredScope is matched against the api_token's granted scopes.
	RequiredScope string
	// RequiredCapability is checked via Authz.Can against the PRINCIPAL. Empty
	// means no capability gate beyond the resource check.
	RequiredCapability string
	// Resource resolves the object this call will touch, from its arguments. A
	// resolution error is a refusal, never a fallback to "workspace".
	Resource func(args map[string]any) (ResourceRef, error)
	// Behaviour describes what the tool does to the world. Emitted to clients as
	// MCP tool annotations AND enforced here — see ToolBehaviour.
	Behaviour ToolBehaviour

	// IdempotencyKey derives a stable key from the arguments for a non-idempotent
	// write, so a retried call is applied once.
	//
	// REQUIRED for any tool that mutates and does not declare itself idempotent,
	// enforced at registration. The reason is specific: MCP clients are told they
	// MAY retry automatically when a tool advertises idempotentHint, and the spec
	// itself says annotations are not guaranteed to describe behaviour faithfully.
	// A server that advertises a property it does not enforce is trusting the
	// client to protect the server's data, which is backwards.
	IdempotencyKey func(args map[string]any) (string, error)
	// Handler performs the work, in terms of an existing business function.
	Handler func(ctx context.Context, in ToolCallContext) (any, error)
}

// ToolBehaviour is what a tool does to the world, in the vocabulary MCP clients
// already understand (readOnlyHint, destructiveHint, idempotentHint).
//
// Declared once and used twice: advertised to clients as annotations, and enforced
// server-side. Both matter, because the MCP specification is explicit that
// annotations are hints which clients must treat as untrusted — so a client may
// ignore them, and a client told a tool is idempotent may retry it on its own.
// Whatever we advertise, we have to be able to survive.
//
// Zero value means the safest reading: not read-only, not idempotent, and
// therefore requiring an idempotency key. A tool that forgets to describe itself
// is treated as dangerous rather than harmless.
type ToolBehaviour struct {
	// ReadOnly means the tool performs no mutation. Reads and writes are budgeted
	// separately, because a retry loop is an agent's normal failure mode.
	ReadOnly bool
	// Destructive means it mutates or removes durable state (delete, archive,
	// overwrite) and is NOT made safe by calling it again.
	Destructive bool
	// Idempotent means applying it twice with the same arguments has the same
	// effect as applying it once, by nature rather than by bookkeeping.
	Idempotent bool
}

// needsIdempotencyKey reports whether this behaviour requires server-side
// deduplication: it changes state, and it is not naturally safe to repeat.
func (b ToolBehaviour) needsIdempotencyKey() bool {
	return !b.ReadOnly && !b.Idempotent
}

// registry holds the registered tools. Guarded because registration happens at
// init and reads happen per request.
var (
	registryMu sync.RWMutex
	registry   = map[string]*ToolSpec{}
)

// Register publishes a tool, refusing any spec that would weaken an invariant.
//
// Returns an error rather than panicking so a caller can register from a table and
// surface a clear failure. The checks are the enforcement of this file's contract:
// without them the invariants are conventions, and conventions are how a guard
// stops holding.
func Register(spec *ToolSpec) error {
	if spec == nil {
		return fmt.Errorf("mcp: nil tool spec")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("mcp: tool has no name")
	}
	if strings.TrimSpace(spec.Description) == "" {
		return fmt.Errorf("mcp: tool %q has no description; a model cannot use a tool it cannot understand", name)
	}
	if len(spec.InputSchema) == 0 {
		return fmt.Errorf("mcp: tool %q has no input schema; unvalidated arguments reach the handler otherwise", name)
	}
	if !json.Valid(spec.InputSchema) {
		return fmt.Errorf("mcp: tool %q has an invalid input schema", name)
	}
	if strings.TrimSpace(spec.RequiredScope) == "" {
		return fmt.Errorf("mcp: tool %q declares no required scope; every tool must be gated by a token scope", name)
	}
	// The critical one. Without a Resource function there is nothing to authorise
	// against, and the tool would run on the strength of its scope alone — which is
	// exactly the confused-deputy path this package exists to close.
	if spec.Resource == nil {
		return fmt.Errorf("mcp: tool %q declares no Resource function, so no per-resource "+
			"authority check is possible; every tool must declare what it touches", name)
	}
	if spec.Handler == nil {
		return fmt.Errorf("mcp: tool %q has no handler", name)
	}
	// A tool cannot be both read-only and destructive; that combination means the
	// author has not decided what it does, and the annotations we emit would lie.
	if spec.Behaviour.ReadOnly && spec.Behaviour.Destructive {
		return fmt.Errorf("mcp: tool %q is declared both read-only and destructive", name)
	}
	// The enforcement that matters. A state-changing tool that is not naturally
	// idempotent MUST be able to deduplicate its own retries, because MCP clients
	// are permitted to retry and the specification treats annotations as untrusted
	// hints. Without a key, a dropped response silently becomes two writes.
	if spec.Behaviour.needsIdempotencyKey() && spec.IdempotencyKey == nil {
		return fmt.Errorf("mcp: tool %q mutates state and is not idempotent, so it must "+
			"provide IdempotencyKey; MCP clients may retry a call whose response was "+
			"lost, and a retry without a key is a duplicate write", name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		return fmt.Errorf("mcp: tool %q is already registered", name)
	}
	registry[name] = spec
	return nil
}

// Lookup returns a registered tool. The second result is false for an unknown
// name, which callers must treat as a refusal rather than an error to describe —
// a detailed "no such tool" reply lets a client enumerate the catalogue it was not
// granted.
func Lookup(name string) (*ToolSpec, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	spec, ok := registry[strings.TrimSpace(name)]
	return spec, ok
}

// There was a VisibleTools(granted) registry filter here, sorted by name. Like AdmittedTools it
// existed only for protocol.go's removed ListTools catalogue, and it was a loop around hasScope.
// The shipped catalogue applies the scope rule inline via ScopeForTool/HasScope over
// ai.ToolRegistry — see listToolsForScopes — and iterates that registry in its own order.

// hasScope reports whether granted contains required. Kept local and exact-match:
// no prefix or wildcard matching, because "channels:*" quietly granting
// "channels:delete" is the kind of convenience that becomes an incident.
func hasScope(granted []string, required string) bool {
	required = strings.TrimSpace(required)
	if required == "" {
		return false
	}
	for _, g := range granted {
		if strings.EqualFold(strings.TrimSpace(g), required) {
			return true
		}
	}
	return false
}

// resetRegistryForTest lives in registryReset_test.go: it is a test fixture, so it belongs
// in test code rather than in the shipped binary.
