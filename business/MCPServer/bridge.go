package business

// Bringing the SHIPPED tools under the governed authorizer.
//
// WHY THIS FILE EXISTS AT ALL. OneCamp has exposed an MCP endpoint at /v1/mcp for
// some time, serving ~25 tools out of the in-app AI registry. It is authenticated
// and scope-gated, and each tool's executor enforces the app's own permission
// model. What it does not have is a single place that decides authority: there is
// no per-object check before the handler runs, no call budget, and no audit record
// of a REFUSAL — only of a successful write.
//
// The wrong fix would be a second MCP server with better properties. That is how a
// codebase ends up with two permission models that drift until one of them is
// wrong, which is the exact failure reach.go's header warns about. So instead of
// standing up a parallel surface, this adapts the tools that already ship onto the
// governed path, one tool at a time, keeping one endpoint and one decision point.
//
// GENERIC BY CONSTRUCTION. Adding a tool here is one table row. The resource
// resolver, the argument coercion, the schema and the handler are each generated
// from that row by one function, so there is no per-tool code in which a per-tool
// authorization mistake could live. A tool either declares what it touches or it
// cannot be registered.
//
// READS ONLY, DELIBERATELY. Every binding below is read-only. The writes in the
// existing registry (create_task, send_message, send_dm, create_doc, ...) name
// objects this package has no authority rule for yet — a project, a DM, a table —
// and a write is exactly the wrong place to guess. They keep their current
// behaviour until those rules exist, and the ratchet test in bridge_test.go lists
// them explicitly so the gap is visible rather than forgotten.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// maxToolOutputRunes bounds what one tool call may return.
//
// Not a performance guard. A tool result is fed to a model as context, and an
// unbounded result is both a cost the caller did not agree to and a larger surface
// for instructions embedded in workspace content to reach that model. Runes rather
// than bytes so the cut never lands mid-character.
const maxToolOutputRunes = 20000

// binding is one existing tool, plus the authority declaration it was missing.
//
// The whole point is that this is DATA. A reviewer can read the table and see every
// tool's resource kind at once, which is not true of twenty-five hand-written
// specs.
type binding struct {
	// Tool is the name in ai.ToolRegistry. Must be a STATIC registry entry.
	Tool string
	// Kind is the resource whose authority governs this tool.
	Kind ResourceKind
	// IDArg is the argument naming the object. Empty ONLY for ResourceWorkspace,
	// enforced at registration.
	IDArg string
	// Behaviour is advertised to clients and enforced here.
	Behaviour ToolBehaviour
}

// bridgedTools is the migration frontier: the tools now governed per object.
//
// Each entry means "authority over this kind is what permits this call", and that
// claim is checked against the tool's real arguments, not its name.
var bridgedTools = []binding{
	// Object-scoped reads. The argument naming the object is the argument the
	// authorizer resolves, so a caller cannot name one doc and read another.
	{Tool: "read_doc", Kind: ResourceDoc, IDArg: "doc_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "summarize_channel", Kind: ResourceChannel, IDArg: "channel_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	// Both take the project as their target, so both are authorised against it.
	// list_project_tasks lists the tasks OF a named project, which makes the project
	// the object under authority — not each task it returns. Membership of the
	// project is what the in-app path requires to see either, so nothing widens.
	{Tool: "read_project", Kind: ResourceProject, IDArg: "project_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_project_tasks", Kind: ResourceProject, IDArg: "project_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// Table reads. All three name the table they act on, and all three are
	// authorised by the table's own visibility rule via dataTableBusiness.CanView.
	// query_table and query_plan aggregate rather than return rows, but they read
	// the same table and so answer to the same authority — an aggregate over data
	// you may not see is still a read of it.
	{Tool: "read_table", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "query_table", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "query_plan", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// EXTERNAL DATA SOURCES — the reads that leave OneCamp entirely.
	//
	// Held back longest of all the reads, and rightly: a mistake here exposes a
	// customer's warehouse rather than a channel. Three of them name the source they act
	// on, and all three answer to the source's own query rule via
	// dataSourceBusiness.CanQuery — an admin, its creator, or the source being
	// workspace-visible, and the source being enabled.
	//
	// query_data_source and query_data_source_plan aggregate rather than return rows, and
	// like the table queries they answer to the same authority as reading: an aggregate
	// over data you may not see is still a read of it.
	//
	// A CORRECTION WORTH RECORDING. The pending note for these said authority was the
	// agent.manage capability. It is not — agent.manage gates the CONFIG routes at the
	// router (registering, editing, deleting a connection). Governing a QUERY with it
	// would have been a stricter and materially different rule than the app's, so the
	// note was not merely vague, it pointed at the wrong rule.
	{Tool: "read_data_source", Kind: ResourceDataSource, IDArg: "data_source_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "query_data_source", Kind: ResourceDataSource, IDArg: "data_source_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "query_data_source_plan", Kind: ResourceDataSource, IDArg: "data_source_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	// list_data_sources names no source — it answers "which can I query at all" — so it
	// is workspace-scoped, and its executor narrows to the caller via ListQueryable,
	// which applies the identical per-source visibility rule to every row it returns.
	// That narrowing in the handler is the obligation ResourceWorkspace documents.
	{Tool: "list_data_sources", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// Group chat: the grouping is the object, and participation is the authority.
	{Tool: "summarize_group_chat", Kind: ResourceChat, IDArg: "grp_id",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// DM: named by the OTHER PERSON, which is its own kind rather than a grouping.
	//
	// It was ResourceWorkspace, on the reasoning that the caller is a participant by
	// construction so there was nothing per-object to check. True as far as it went,
	// but it left the tool with no per-object decision at all — and ResourceWorkspace
	// cannot express a write, so it could never have carried send_dm. ResourceDirectMessage
	// says the same thing about the caller's side and adds the half that was missing:
	// whether the person on the other end can be reached.
	{Tool: "summarize_dm", Kind: ResourceDirectMessage, IDArg: "to_user_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// Workspace-scoped reads. These name no single object; each one's executor
	// already narrows its results to the calling person, which is the obligation
	// ResourceWorkspace documents. They are listed here rather than left ungoverned
	// so they still get budget and an audited refusal.
	{Tool: "list_tasks", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_projects", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_teams", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_tables", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},

	// THE FIRST GOVERNED WRITE.
	//
	// Chosen because it is the safest mutation in the registry and its authority rule
	// was already written and tested: the ResourceTask branch requires project ADMIN
	// for a write, which is what the shipped task controllers require to change a
	// status.
	//
	// Idempotent BY NATURE, not by bookkeeping — setting a task to "done" twice leaves
	// it done — so it needs no idempotency key, which is why registration accepts it
	// without one. That matters because MCP clients are permitted to retry a call whose
	// response was lost, and a status change is one of the few writes where a retry is
	// genuinely harmless.
	//
	// NOT destructive: it overwrites a field, but the previous value is recoverable by
	// setting it back and a repeat is a no-op. Destructive is reserved for changes a
	// second call does not make safe, which is the class that needs a human.
	{Tool: "update_task_status", Kind: ResourceTask, IDArg: "task_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: true}},

	// The rest of the NATURALLY IDEMPOTENT writes. Each one sets a field to a value,
	// so applying it twice leaves exactly the state applying it once leaves — which is
	// what Idempotent means here, by nature rather than by bookkeeping. They need no
	// deduplication key for the same reason update_task_status does not, and a retry
	// after a lost response is genuinely harmless.
	//
	// None of them is Destructive: a field is overwritten, the previous value is
	// recoverable by setting it back, and a repeat is a no-op. Destructive is reserved
	// for changes a second call does not make safe.
	//
	// Their authority rules were already written and tested, so these are wiring
	// rather than new policy: task writes require project ADMIN (the same rule the
	// shipped task controllers apply to a mutation) and table writes go through
	// dataTableBusiness.CanManage.
	//
	// STILL TO COME for assign_task: the ASSIGNEE is a second person this call
	// affects, and nothing here verifies they are someone who can hold work — the
	// same recipient question the ResourceChat branch documents for sends. Governing
	// the task is a strict improvement over the ungoverned path either way, and the
	// recipient rule lands with the send tools rather than being half-applied here.
	{Tool: "assign_task", Kind: ResourceTask, IDArg: "task_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: true}},
	{Tool: "set_task_due_date", Kind: ResourceTask, IDArg: "task_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: true}},
	// The table is the object under authority, not the row: CanManage is granted over
	// a table, and a row cannot be reached except through it.
	{Tool: "update_table_row", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: true}},
	// Linking a row twice, or unlinking it twice, leaves it as once: a link is
	// there or it isn't. Unlinking is undone by linking again.
	{Tool: "link_table_rows", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: true}},

	// THE FIRST WRITES THAT ARE NOT SAFE TO REPEAT. Calling either twice produces two
	// rows or two messages, so each retry a client makes after a lost response would
	// duplicate real content. They are the reason the claim/settle machinery in
	// claim.go exists: their identity comes from their arguments, so the second
	// arrival of the same call is recognised and answered without running again.
	//
	// Additive rather than destructive — they add content and remove none — so they do
	// not ask a human first. Prompting for approval on every message an agent sends
	// would train people to click Approve without reading, which is worse than not
	// asking; the audit row and the channel itself are the review.
	{Tool: "create_table_row", Kind: ResourceTable, IDArg: "table_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},
	// Posting is governed by the channel's own rule, which already separates reading
	// from posting: any member may read, and an admins-only channel refuses a write
	// from a non-admin. That distinction is the shipped one, not a new one.
	//
	// A channel has no recipient to verify — it has members — which is why this send
	// is governable now while send_dm and send_group_chat are not. Those address a
	// PERSON, and the ResourceChat branch documents what they still need.
	{Tool: "send_message", Kind: ResourceChannel, IDArg: "channel_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},

	// THE TWO CREATIONS THAT NAME NOTHING AT ALL.
	//
	// create_doc(title, is_private, body) and set_reminder(title, start_time, description)
	// take no container argument. Each makes something that belongs to the caller and
	// sits in no parent, so there is genuinely no object for an authority rule to check —
	// which is why they were the last two, and why the answer is a kind that says so
	// plainly rather than one that pretends otherwise. See ResourceSelfOwned.
	//
	// THE GAIN IS THE WRITE MACHINERY, not a new permission check. On the ungoverned path
	// these had no idempotency key, so a client's retry after a lost response made two
	// documents or two reminders. They are non-idempotent creations, so they now get the
	// generated key and a retry is recognised.
	//
	// Both remain bounded by their token scope (docs:write, calendar:write) and by
	// everything else the ladder applies. The only check they do not get is the one that
	// has no meaning for them.
	{Tool: "create_doc", Kind: ResourceSelfOwned,
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},
	{Tool: "set_reminder", Kind: ResourceSelfOwned,
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},

	// THE TWO CREATIONS THAT NAME THEIR CONTAINER.
	//
	// A create names no object of its own, so it is authorised against the thing it
	// creates INTO — and for both of these the container's rule already existed and
	// already required exactly what the shipped create path requires.
	//
	// create_task -> the project, whose write rule is project ADMIN. That is precisely
	// what executeCreateTask enforces. The pending note claimed no ResourceKind could
	// express this; ResourceProject's write rule had expressed it since the Access
	// dimension shipped, so the note was simply wrong.
	//
	// create_project -> the team, via the new ResourceTeam, whose write rule is team
	// ADMIN — what executeCreateProject enforces.
	//
	// Both are NON-IDEMPOTENT: calling either twice makes two things. They get the
	// generated deduplication key, so a client's retry after a lost response is
	// recognised rather than creating a second task or project.
	{Tool: "create_task", Kind: ResourceProject, IDArg: "project_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},
	{Tool: "create_project", Kind: ResourceTeam, IDArg: "team_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},

	// THE TWO SENDS THAT ADDRESS PEOPLE. Held back until now because a send has a
	// recipient, and no rule accounted for the other side of the exchange.
	//
	// send_dm names a person, so ResourceDirectMessage carries exactly that question:
	// resolvable, not deactivated, and not an attribution-only ghost — with bots
	// permitted, because the automation bot is meant to be messageable. The rule is the
	// shipped one, now held in principalBusiness.CanReceiveDirectMessage so the HTTP
	// controller, the in-app executor and this authorizer cannot diverge. They had
	// diverged: the executor was missing the external test the controller enforced.
	//
	// send_group_chat needs nothing beyond ResourceChat's write rule, which working it
	// through established rather than assumed. A group send names a ROOM the caller is
	// already shown to be in, so there is no per-recipient decision — the same as
	// posting to a channel — and the participant list it delivers to is read by a query
	// that already excludes external identities.
	{Tool: "send_dm", Kind: ResourceDirectMessage, IDArg: "to_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},
	{Tool: "send_group_chat", Kind: ResourceChat, IDArg: "grp_id",
		Behaviour: ToolBehaviour{ReadOnly: false, Destructive: false, Idempotent: false}},

	// The one an agent reaches for first, and so the one most worth governing.
	// Workspace-scoped because it names no object: it takes a query and fans out
	// across messages, docs, tasks, memory and connected apps. Its executor
	// resolves the calling person and hands that identity to UnifiedSearch, which
	// is exactly the narrowing ResourceWorkspace requires of a handler — the
	// authorizer cannot do it, so the handler must, and here it does.
	{Tool: "search_workspace", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
}

// RegisterBridgedTools publishes every binding as a governed tool.
//
// Called once at startup. Returns on the first failure rather than registering a
// partial catalogue, because a half-registered surface is one where a client's
// tools/list result depends on how far startup got.
func RegisterBridgedTools() error {
	for _, b := range bridgedTools {
		spec, err := specFor(b)
		if err != nil {
			return err
		}
		if err := Register(spec); err != nil {
			return err
		}
	}
	return nil
}

// specFor turns one binding into a governed ToolSpec. The single place where a
// bridged tool's authorization is assembled.
func specFor(b binding) (*ToolSpec, error) {
	def, ok := staticToolDef(b.Tool)
	if !ok {
		return nil, fmt.Errorf("mcp: %q is not a static tool in ai.ToolRegistry; only "+
			"compile-time tools may be bridged, because a dynamic tool's description and "+
			"schema are derived from workspace content and would carry it into a model's "+
			"context as trusted instructions", b.Tool)
	}
	// Behaviour must match the registry, or we would advertise one thing and run
	// another. The registry's ReadOnly flag is what the in-app agent loop trusts to
	// decide it may auto-run a tool without asking, so a disagreement here is a
	// disagreement about whether a human must approve.
	if def.ReadOnly != b.Behaviour.ReadOnly {
		return nil, fmt.Errorf("mcp: %q is ReadOnly=%v in ai.ToolRegistry but "+
			"ReadOnly=%v in its binding; these must agree", b.Tool, def.ReadOnly, b.Behaviour.ReadOnly)
	}
	// A tool whose effect leaves the workspace irrecoverably cannot be carried by a
	// bearer credential, because the thing it needs is a person confirming it and a
	// token cannot be one. Refused at startup rather than caught in review: this
	// surface has no way to demand approval BEFORE a call runs, so exposing such a
	// tool would mean the mail is already sent by the time anyone is asked.
	//
	// Unreachable today via the scope check below (these tools are absent from
	// ToolScope), and stated separately anyway — the scope map's job is naming which
	// permission guards a tool, not carrying this reason, and a future edit that adds
	// a scope should still fail here.
	if def.ExternalEffect {
		return nil, fmt.Errorf("mcp: %q has an effect that leaves the workspace and cannot be undone, so "+
			"it needs a human to confirm it and a bearer token cannot be one; it must not be exposed "+
			"until this surface can require approval before the call runs", b.Tool)
	}
	// Scope comes from the existing map, never re-declared. Two sources for "which
	// scope guards this tool" is two answers the first time one is edited.
	scope, public := apiTokenBusiness.ScopeForTool(b.Tool)
	if !public {
		return nil, fmt.Errorf("mcp: %q has no scope in apiTokenBusiness.ToolScope, so it "+
			"is not part of the public API surface and must not be exposed", b.Tool)
	}
	schema, err := schemaFor(def)
	if err != nil {
		return nil, err
	}
	resolver, err := resourceResolver(b)
	if err != nil {
		return nil, err
	}

	return &ToolSpec{
		Name:          b.Tool,
		Description:   def.Description,
		InputSchema:   schema,
		RequiredScope: scope,
		Resource:      resolver,
		Behaviour:     b.Behaviour,
		// Derived, never hand-written per tool. Nil for a read or a naturally
		// idempotent write, which is what Register expects for those.
		IdempotencyKey: idempotencyKeyFor(def, b.Behaviour),
		Handler:        handlerFor(b.Tool),
	}, nil
}

// idempotencyKeyFor builds the deduplication-key function for a bridged tool, or nil
// when the behaviour needs none.
//
// THE IDENTITY OF A CALL IS ITS ARGUMENTS. "Send this text to this channel" is the
// same intent however many times a client resends it after losing the response, so the
// key is derived from the declared arguments and from nothing else. Never from a
// counter, a timestamp or a random value: a key that differs per attempt deduplicates
// nothing, which is the mistake this whole mechanism exists to avoid.
//
// GENERATED FROM THE REGISTRY, so there is no per-tool key function in which a
// per-tool mistake could live — the same reason the resource resolver is generated.
// Adding a non-idempotent tool is still one table row.
//
// DECLARED PARAMETERS ONLY, in registry order. Two properties follow, and both are
// wanted. Order is fixed by the registry rather than by map iteration, so the same
// arguments always hash alike. And an argument the tool does not declare cannot change
// a call's identity, so a client adding a field its executor ignores does not turn a
// retry into a new write.
//
// The caller (DeriveIdempotencyKey) additionally binds this to the tool, the resource
// and the principal, so two tools or two people cannot collide.
func idempotencyKeyFor(def ai.ToolDef, behaviour ToolBehaviour) func(map[string]any) (string, error) {
	if !behaviour.needsIdempotencyKey() {
		return nil
	}
	// Copy the parameter names at registration so the closure cannot observe a later
	// mutation of the registry.
	names := make([]string, 0, len(def.Parameters))
	for _, p := range def.Parameters {
		names = append(names, p.Name)
	}

	return func(args map[string]any) (string, error) {
		var b strings.Builder
		for _, name := range names {
			// asString, so the key is derived from the EXACT value the executor will
			// receive. Hashing the raw JSON instead would let 5 and 5.0 look like
			// different calls that then perform the identical write.
			v := asString(args[name])
			// Length-prefixed so no combination of values can be confused with
			// another: "ab" + "c" must not hash like "a" + "bc".
			fmt.Fprintf(&b, "%d:%s%d:%s", len(name), name, len(v), v)
		}
		out := b.String()
		if strings.TrimSpace(out) == "" {
			// Every non-idempotent tool in the registry declares at least one required
			// argument, so this means the registry changed shape. Refusing beats
			// returning a constant key, which would make every call to this tool look
			// like a duplicate of the first.
			return "", fmt.Errorf("mcp: %q declares no arguments to derive an identity from", def.Name)
		}
		return out, nil
	}
}

// staticToolDef finds a tool in the COMPILE-TIME registry only.
//
// Deliberately not ai.GetExecutor or ai.ToolIsReadOnly: both fall through to the
// dynamic registry, and a lookup that can silently resolve a workspace-defined tool
// is not a safe basis for publishing a description to a model.
func staticToolDef(name string) (ai.ToolDef, bool) {
	for _, t := range ai.ToolRegistry {
		if t.Name == name {
			return t, true
		}
	}
	return ai.ToolDef{}, false
}

// schemaFor renders a tool's parameters as JSON Schema.
//
// Safe as static text because it is built purely from ai.ToolRegistry, whose
// entries are compile-time constants — staticToolDef is what guarantees that.
func schemaFor(def ai.ToolDef) (json.RawMessage, error) {
	props := map[string]any{}
	required := []string{}
	for _, p := range def.Parameters {
		jsType := "string"
		if p.Type == "boolean" {
			jsType = "boolean"
		}
		props[p.Name] = map[string]any{"type": jsType, "description": p.Description}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("mcp: could not build input schema for %q: %w", def.Name, err)
	}
	return raw, nil
}

// resourceResolver builds the function that names the object a call will act on.
//
// ONE implementation for every bridged tool. The alternative — a hand-written
// resolver per tool — is twenty-five chances to read the wrong argument, and
// reading the wrong argument means authorising against an object the call will not
// actually touch.
func resourceResolver(b binding) (func(map[string]any) (ResourceRef, error), error) {
	kind, idArg := b.Kind, strings.TrimSpace(b.IDArg)

	// Kinds that name no single object: nothing to resolve, and an id argument would be
	// a contradiction. Asked via the predicate rather than by listing the kinds, so a
	// third such kind cannot be added and forgotten here.
	if kind.IdentifiesNoObject() {
		if idArg != "" {
			return nil, fmt.Errorf("mcp: %q is scoped to a %s, which names no single object, "+
				"but declares an id argument %q; there is nothing for that id to "+
				"authorise against", b.Tool, kind, idArg)
		}
		return func(map[string]any) (ResourceRef, error) {
			return ResourceRef{Kind: kind}, nil
		}, nil
	}
	if idArg == "" {
		return nil, fmt.Errorf("mcp: %q is scoped to a %s but names no id argument, so there "+
			"is nothing to authorise against", b.Tool, kind)
	}
	// Confirm the argument actually exists on the tool. Otherwise a typo here would
	// produce a resolver that always fails, and the tool would be permanently and
	// silently unusable.
	def, _ := staticToolDef(b.Tool)
	found := false
	for _, p := range def.Parameters {
		if p.Name == idArg {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("mcp: %q declares id argument %q, which is not one of its "+
			"parameters", b.Tool, idArg)
	}

	return func(args map[string]any) (ResourceRef, error) {
		id := strings.TrimSpace(asString(args[idArg]))
		if id == "" {
			// A refusal, not a fallback to workspace scope. Widening the scope of a
			// call because its target was missing is how an object-scoped tool
			// becomes a workspace-scoped one.
			return ResourceRef{}, fmt.Errorf("%s is required", idArg)
		}
		return ResourceRef{Kind: kind, ID: id}, nil
	}, nil
}

// handlerFor adapts the existing executor to the governed handler signature.
//
// The executor runs as in.PrincipalUserID — the human who created the credential —
// and never as the token or an admin. That is the same identity the authorizer just
// checked, passed through rather than re-derived, so the call cannot execute as
// somebody other than the person it was authorised for.
func handlerFor(tool string) func(context.Context, ToolCallContext) (any, error) {
	return func(ctx context.Context, in ToolCallContext) (any, error) {
		// Resolved at call time, not registration time: executors are wired into
		// ai.Executors during startup to avoid an import cycle, so a registration-time
		// lookup would run before they exist.
		executor, ok := ai.GetExecutor(tool)
		if !ok {
			return nil, fmt.Errorf("mcp: no executor registered for %q", tool)
		}
		text, meta, err := executor(ctx, ai.ProposedAction{
			ToolName: tool,
			Params:   StringifyArgs(in.Args),
		}, in.PrincipalUserID)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"text": helpers.TruncateRunes(text, maxToolOutputRunes),
			"meta": meta,
		}, nil
	}
}

// StringifyArgs coerces JSON argument values to the string map executors take.
//
// Exported because the legacy path needs the identical coercion: if the two
// disagreed, a tool could receive different arguments depending on which path
// called it, and the authorization decision was made about the governed reading.
func StringifyArgs(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = asString(v)
	}
	return out
}

// asString renders one JSON value the way the executors expect.
func asString(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode to float64; render whole numbers without a decimal so
		// an id or a count does not arrive as "50.000000".
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
