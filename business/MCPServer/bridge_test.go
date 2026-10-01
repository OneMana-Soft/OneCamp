package business

import (
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
)

// Registration is where every binding's claims are checked against the real tool
// registry: that the tool exists and is static, that its id argument is one of its
// actual parameters, that its scope is declared, and that its behaviour matches.
//
// So this test is not "does registration work" — it is "is every row in the table
// true". A typo in an id argument produces a tool that can never resolve its
// resource and is therefore permanently refused, which is the kind of failure that
// otherwise surfaces as a confused bug report months later.
func TestEveryBridgedToolRegisters(t *testing.T) {
	resetRegistryForTest()
	if err := RegisterBridgedTools(); err != nil {
		t.Fatalf("bridged tools failed to register: %v", err)
	}
	for _, b := range bridgedTools {
		if _, ok := Lookup(b.Tool); !ok {
			t.Errorf("%s registered without error but is not in the registry", b.Tool)
		}
	}
}

// The migration frontier, made explicit.
//
// apiTokenBusiness.ToolScope is the set of tools reachable over the public API and
// therefore over /v1/mcp. Every one of them is either governed by this package or
// still on the legacy path, and this test forces that to be a stated choice.
//
// It exists because the dangerous case is silent: someone adds a tool to ToolScope,
// it becomes callable over MCP immediately with only a scope check, and nothing
// anywhere notices it never got an authority rule. Now it fails here, at the point
// where the decision is being made.
func TestEveryPublicToolIsGovernedOrExplicitlyPending(t *testing.T) {
	governed := map[string]bool{}
	for _, b := range bridgedTools {
		governed[b.Tool] = true
	}

	// Tool -> why it is not governed yet. Every entry names a rule this package does not
	// express; none is "we did not get round to it".
	//
	// CURRENTLY EMPTY: every publicly exposable tool is governed. Keep the map — it is
	// where a NEW ungoverned tool declares why, and the assertions below use it in both
	// directions: a public tool absent from both the registry and this map fails, and an
	// entry for a tool that has since been governed fails as stale. Empty means the
	// surface has no unguarded corner, not that the check has been retired.
	//
	// list_tables_dynamic is deliberately not listed: it is not publicly exposable at all
	// (no entry in apiTokenBusiness.ToolScope) because its description and schema derive
	// from workspace content and would reach a model as trusted instruction. The loop
	// below walks only the public catalogue, so it never asks about it.
	pending := map[string]string{}

	// THE PENDING LIST MUST NOT LIE. An entry for a tool that has since been governed
	// is stale, and a stale entry is worse than no entry: this list is where a reviewer
	// looks to find out what is still unguarded, and a reason written for a tool that
	// no longer needs one makes the gap look bigger than it is while hiding whether
	// anyone has revisited the rest.
	for tool, reason := range pending {
		if governed[tool] {
			t.Errorf("tool %q is governed by a binding but is still listed as pending "+
				"(%q). Remove the entry: this list is the record of what remains "+
				"unguarded, and it is only useful if it is true.", tool, reason)
		}
	}

	for tool := range apiTokenBusiness.ToolScope {
		if governed[tool] {
			continue
		}
		if reason, ok := pending[tool]; ok {
			t.Logf("not yet governed: %s (%s)", tool, reason)
			continue
		}
		t.Errorf("tool %q is callable over /v1/mcp but is neither governed by this package "+
			"nor listed as pending. Either add a binding — which requires deciding what "+
			"authority over it means — or add it to the pending map with the reason. A tool "+
			"reaching the MCP surface with only a scope check and no per-object authority "+
			"rule is the confused-deputy path this package exists to close.", tool)
	}
}

// What a bridged WRITE must satisfy.
//
// Reads only had to name their object. A write has two further obligations, and both
// exist because of how MCP clients behave rather than because of anything about our
// code:
//
//  1. RETRY SAFETY. The specification permits a client to retry a call whose response
//     was lost, and treats the annotations we publish as untrusted hints — so a client
//     may retry whatever we say. A write must therefore either be idempotent by nature
//     or carry a key that lets the server collapse the duplicate. Registration enforces
//     this; asserting it here states it as an intended property of the table rather
//     than a coincidence of what registration happens to check.
//
//  2. A DESTRUCTIVE TOOL MUST BE ABLE TO BE APPROVED. The transport now routes such a
//     call into the in-thread Approve/Deny card, so a destructive tool no longer has to
//     be kept out of this table — but it does have to carry an idempotency key, and for
//     a reason specific to approval rather than to retries: the key is what lets a
//     client's second attempt find the card its first attempt raised, instead of
//     stacking a new card per retry and asking a human the same question repeatedly.
//     A destructive tool that is "idempotent by nature" therefore still needs a key,
//     which is what this asserts.
func TestBridgedWritesAreRetrySafeAndNotDestructive(t *testing.T) {
	for _, b := range bridgedTools {
		if b.Behaviour.ReadOnly {
			if b.Behaviour.Destructive {
				t.Errorf("%s is declared both read-only and destructive", b.Tool)
			}
			continue
		}

		spec, err := specFor(b)
		if err != nil {
			t.Errorf("%s: %v", b.Tool, err)
			continue
		}
		if !b.Behaviour.Idempotent && spec.IdempotencyKey == nil {
			t.Errorf("%s mutates state, is not idempotent, and provides no IdempotencyKey. "+
				"An MCP client may retry a call whose response was lost, so without one a "+
				"dropped response becomes two writes.", b.Tool)
		}
		if b.Behaviour.Destructive && spec.IdempotencyKey == nil {
			t.Errorf("%s is a DESTRUCTIVE write with no IdempotencyKey. The key is how a "+
				"retry finds the approval card the first attempt raised; without one, every "+
				"retry raises another card and a human is asked the same question until they "+
				"stop reading them.", b.Tool)
		}
	}
}

// A governed write must be authorised by a rule that actually distinguishes writing
// from reading, or it inherits a visibility answer to a modification question.
//
// Checked as a property of the KIND rather than of the tool, because the distinction
// lives in the reach branch. ResourceWorkspace is the interesting case: it refuses
// writes outright, so a write bridged to it would always be refused — which is safe
// but means a permanently broken tool, and is worth catching here rather than in a
// bug report.
func TestBridgedWritesNameAKindThatCanExpressThem(t *testing.T) {
	for _, b := range bridgedTools {
		if b.Behaviour.ReadOnly {
			continue
		}
		if b.Kind == ResourceWorkspace {
			t.Errorf("%s is a write bridged to ResourceWorkspace, which refuses every write "+
				"because there is no object to authorise against. The tool would be "+
				"permanently refused; a write must name what it changes.", b.Tool)
		}
	}
}

// An object-scoped tool whose target is missing must REFUSE, never widen to
// workspace scope. Widening is how a tool that reads one document becomes a tool
// that reads whatever the handler defaults to.
func TestObjectScopedResolverRefusesAMissingID(t *testing.T) {
	var docBinding binding
	for _, b := range bridgedTools {
		if b.Kind == ResourceDoc {
			docBinding = b
			break
		}
	}
	if docBinding.Tool == "" {
		t.Skip("no doc-scoped binding to exercise")
	}

	resolve, err := resourceResolver(docBinding)
	if err != nil {
		t.Fatalf("building resolver: %v", err)
	}

	for _, args := range []map[string]any{
		{},
		{docBinding.IDArg: ""},
		{docBinding.IDArg: "   "},
		{docBinding.IDArg: nil},
	} {
		ref, err := resolve(args)
		if err == nil {
			t.Fatalf("args %v resolved to %+v instead of refusing; a missing target must "+
				"not become a broader scope", args, ref)
		}
	}

	// And the positive case, so the refusals above are not passing because the
	// resolver never works.
	ref, err := resolve(map[string]any{docBinding.IDArg: "  doc-123  "})
	if err != nil {
		t.Fatalf("a present id was refused: %v", err)
	}
	if ref.Kind != ResourceDoc || ref.ID != "doc-123" {
		t.Fatalf("got %+v, want the doc kind with a trimmed id", ref)
	}
}

// The resolver must reject a binding that names an argument the tool does not have,
// because such a resolver always fails and the tool would be silently dead.
func TestResolverRejectsAnUnknownIDArgument(t *testing.T) {
	_, err := resourceResolver(binding{Tool: "read_doc", Kind: ResourceDoc, IDArg: "not_a_real_param"})
	if err == nil {
		t.Fatal("a binding naming a nonexistent parameter was accepted; the resulting tool " +
			"could never resolve its resource and would be permanently refused")
	}
	if !strings.Contains(err.Error(), "not one of its") {
		t.Fatalf("error should name the problem, got: %v", err)
	}
}

// A workspace-scoped binding must not name an id argument: there is no single object
// to authorise against, so an id would imply a check that does not happen.
func TestWorkspaceBindingMayNotNameAnID(t *testing.T) {
	_, err := resourceResolver(binding{Tool: "list_tasks", Kind: ResourceWorkspace, IDArg: "status"})
	if err == nil {
		t.Fatal("a workspace-scoped binding was allowed to name an id argument")
	}
}

// Argument coercion has to match what the executors already receive, because the
// authorization decision was made about the governed reading of these arguments. If
// the two paths coerced differently, a call could be authorised against one value
// and executed with another.
func TestStringifyArgsMatchesExecutorExpectations(t *testing.T) {
	got := StringifyArgs(map[string]any{
		"s":     "text",
		"whole": float64(50),
		"frac":  1.5,
		"yes":   true,
		"no":    false,
		"nil":   nil,
		"obj":   map[string]any{"k": "v"},
	})
	want := map[string]string{
		"s": "text",
		// The case that matters: a count or an id must not arrive as "50.000000".
		"whole": "50",
		"frac":  "1.5",
		"yes":   "true",
		"no":    "false",
		"nil":   "",
		"obj":   `{"k":"v"}`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("arg %q = %q, want %q", k, got[k], v)
		}
	}
}

// Only compile-time tools may be bridged. A dynamic tool's description and schema
// are built from workspace content, and a tool description reaches a model as
// trusted instruction — so bridging one would hand any workspace author a prompt
// injection channel into every connected agent.
func TestOnlyStaticRegistryToolsMayBeBridged(t *testing.T) {
	if _, err := specFor(binding{Tool: "definitely_not_a_static_tool", Kind: ResourceWorkspace,
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}}); err == nil {
		t.Fatal("a tool absent from ai.ToolRegistry was accepted for bridging")
	}
}

// A tool whose effect leaves the workspace irrecoverably may not be bridged.
//
// gmail_send is the case: OneCamp's in-app assistant can send mail, and every layer
// of this surface would happily carry it — it is a static registry tool, it has a
// schema, and a resolver could be written for it. What it cannot have is the thing it
// actually needs, which is a person confirming that this mail goes to this address
// now. A token proves it was issued; it does not confirm anything. And this surface
// authorises calls, it does not pause them, so by the time anyone could be asked the
// mail has been delivered.
//
// Asserted against the real tool rather than a fixture so it stays true to what the
// registry says, and it is checked BEFORE the scope lookup so the refusal survives
// someone adding a scope for gmail_send — which is the edit that would otherwise
// quietly make it exposable.
func TestExternalEffectToolsMayNotBeBridged(t *testing.T) {
	_, err := specFor(binding{Tool: "gmail_send", Kind: ResourceSelfOwned, IDArg: "",
		Behaviour: ToolBehaviour{ReadOnly: false, Idempotent: false}})
	if err == nil {
		t.Fatal("gmail_send was accepted for bridging: a bearer credential cannot supply the human " +
			"confirmation an unrecallable action requires")
	}
	if !strings.Contains(err.Error(), "leaves the workspace") {
		t.Errorf("refused for the wrong reason, so the guard under test may not be the one that fired: %v", err)
	}
}
