package controllers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	mcpServerBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// TestMain registers the governed catalogue exactly once for the whole package.
//
// Once, because registration is deliberately NOT idempotent: a second Register of
// the same name is an error, since in a running server that means two definitions of
// one tool and no way to know which is authoritative. That is the right behaviour to
// keep, so the tests adapt to it rather than the reverse.
func TestMain(m *testing.M) {
	if err := mcpServerBusiness.RegisterBridgedTools(); err != nil {
		panic("registering bridged tools for tests: " + err.Error())
	}
	os.Exit(m.Run())
}

// The migration frontier has to be observable from the transport, because the
// transport is what decides which path a call takes. If registration silently did
// nothing, isGoverned would answer false for everything and every call would quietly
// keep taking the legacy path — the change would look applied and do nothing.
func TestGovernedToolsAreRecognisedAfterRegistration(t *testing.T) {
	if !isGoverned("read_doc") {
		t.Error("read_doc is bridged but the transport does not recognise it as governed, " +
			"so it would keep taking the legacy path with no per-object authority check")
	}

	// THE NEGATIVE SIDE IS DERIVED, NOT NAMED. Asserting only the positive would pass
	// just as well if isGoverned always returned true, which would route every tool —
	// including ungoverned writes — down a path whose rules were not written for them.
	//
	// It used to name send_dm, and went stale the day send_dm was governed. So it now
	// finds a public tool that is genuinely not bridged, which keeps the assertion
	// honest as the frontier moves.
	ungoverned := ""
	for tool := range apiTokenBusiness.ToolScope {
		if !isGoverned(tool) {
			ungoverned = tool
			break
		}
	}
	if ungoverned == "" {
		// Not a failure: it is the goal. The unknown-name assertion below still proves
		// isGoverned is not simply always true.
		t.Log("every public tool is now governed; the derived negative case has nothing " +
			"left to check")
	} else if isGoverned(ungoverned) {
		t.Errorf("%q is NOT bridged but the transport treats it as governed. A write served "+
			"by the governed path would borrow read-authority rules, which answer 'may this "+
			"person see it' rather than 'may they change it'", ungoverned)
	}
	if isGoverned("") || isGoverned("no_such_tool") {
		t.Error("an unknown tool name was treated as governed")
	}
}

// Whitespace must not decide which authorization path a call takes. " read_doc"
// reaching the legacy path while "read_doc" reaches the governed one would be a
// trivially exploitable way to pick the weaker check.
func TestGovernedLookupIgnoresSurroundingWhitespace(t *testing.T) {
	for _, name := range []string{" read_doc", "read_doc ", "  read_doc  "} {
		if !isGoverned(name) {
			t.Errorf("%q was not recognised as governed; padding a tool name must not "+
				"select the weaker authorization path", name)
		}
	}
}

// The dispatch must come BEFORE the legacy scope gate.
//
// Asserted by reading the source because the ordering is a property of the function
// body and both orders compile. It matters for a specific reason: the governed
// ladder performs the scope check itself as one rung, alongside the per-object,
// budget and audit rungs. Running the legacy gate first would decide nothing the
// full ladder does not already decide, while being able to refuse a call with a
// worse-stated reason and no audit record.
func TestGovernedDispatchPrecedesTheLegacyScopeGate(t *testing.T) {
	raw, err := os.ReadFile("mcpServerController.go")
	if err != nil {
		t.Fatalf("read mcpServerController.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	call := strings.Index(src, "func handleToolCall")
	if call < 0 {
		t.Fatal("handleToolCall not found; this test has gone stale")
	}
	body := src[call:]

	governed := strings.Index(body, "isGoverned(")
	if governed < 0 {
		t.Fatal("handleToolCall no longer dispatches to the governed path; every migrated " +
			"tool has silently reverted to a scope-only check with no audited refusal")
	}
	scopeGate := strings.Index(body, "ScopeForTool(")
	if scopeGate >= 0 && scopeGate < governed {
		t.Error("the legacy scope gate runs before the governed dispatch; the governed " +
			"ladder already includes the scope check, so this can only refuse calls " +
			"earlier and with less recorded about them")
	}
}

// A refusal must not be reported as an internal error.
//
// The legacy path answers a missing scope with -32603, which tells a client the
// SERVER failed when in fact it decided. Clients retry server errors — so a
// permission refusal dressed as one produces a retry loop against a decision that
// will never change. The governed codes exist to separate the three cases, and this
// pins them apart.
func TestGovernedRefusalCodesAreDistinctAndNotInternalError(t *testing.T) {
	const jsonRPCInternalError = -32603

	codes := map[string]int{
		"unauthorized": codeGovernedUnauthorized,
		"refused":      codeGovernedRefused,
		"budget":       codeGovernedBudget,
		"tool failed":  codeGovernedToolFailed,
	}
	seen := map[int]string{}
	for name, code := range codes {
		if code == jsonRPCInternalError {
			t.Errorf("%s uses -32603 (internal error); a decision is not a malfunction, and "+
				"clients retry malfunctions", name)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("%s and %s share code %d, so a client cannot tell them apart", name, prev, code)
		}
		seen[code] = name
	}
}

// Every governed tool must still be part of the public API surface.
//
// The governed path bypasses the legacy ScopeForTool gate, so if a tool were
// governed but absent from ToolScope it would be reachable with no scope requirement
// at all. Registration already refuses that; this asserts it from the side that
// would suffer, so the guarantee is checked where it is relied upon.
// Iterates the SERVED registry and filters with isGoverned, rather than the spec registry.
//
// This used to call mcpServerBusiness.VisibleTools, which was removed along with the unreachable
// ListTools catalogue it existed for. Going through ai.ToolRegistry + isGoverned is a better test
// of the same guarantee anyway: it walks the frontier the endpoint actually routes on (see
// handleToolCall), so a tool that is governed but missing a public scope is caught in the exact
// terms the serving path uses.
func TestEveryGovernedToolStillHasAPublicScope(t *testing.T) {
	checked := 0
	for _, tool := range ai.ToolRegistry {
		if !isGoverned(tool.Name) {
			continue
		}
		checked++
		if _, public := apiTokenBusiness.ScopeForTool(tool.Name); !public {
			t.Errorf("governed tool %q has no entry in apiTokenBusiness.ToolScope. The "+
				"governed path skips the legacy scope gate, so such a tool would be callable "+
				"with no scope requirement at all", tool.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no governed tools were found in ai.ToolRegistry — this check would pass vacuously")
	}
}

// allScopes was here to widen a token for VisibleTools. Both are gone: the check above walks
// ai.ToolRegistry directly, so there is no token to widen.

// A mutating call must be PLANNED before it is executed.
//
// PlanWrite is where "run it", "a person must approve first", "this was already
// applied" and "refused" are decided. All four are meaningless after the fact: a write
// that has already happened cannot be deferred for approval, and cannot be recognised
// as a duplicate of itself.
//
// Asserted by reading the source because both orders compile and the difference is
// invisible from outside without a destructive tool to fire — which is deliberately
// still absent from the bridged table.
func TestMutatingCallsArePlannedBeforeExecution(t *testing.T) {
	src := governedSource(t)

	plan := strings.Index(src, "PlanWrite(")
	if plan < 0 {
		t.Fatal("the governed path no longer calls PlanWrite. Approval, idempotency and " +
			"duplicate detection all live there, so without it a destructive tool executes " +
			"with no second person having seen it and a retried write is applied twice.")
	}
	run := strings.Index(src, "Spec.Handler(")
	if run < 0 {
		t.Fatal("the governed path no longer invokes a handler; this test has gone stale")
	}
	if plan > run {
		t.Error("PlanWrite runs AFTER the handler, so the change has already happened by " +
			"the time anything decides whether it should")
	}
}

// The plan switch must default to a REFUSAL.
//
// WriteOutcome is an open enumeration in practice: a future outcome will be added by
// someone thinking about that outcome, not about this switch. If the default fell
// through to execution, adding a value would silently execute calls nobody had decided
// the meaning of yet.
func TestUnknownWriteOutcomesDoNotExecute(t *testing.T) {
	src := governedSource(t)

	sw := strings.Index(src, "switch plan.Outcome")
	if sw < 0 {
		t.Fatal("the governed path no longer switches on the write plan; this test has gone stale")
	}
	// The switch must end in a default that refuses, before the handler is reached.
	body := src[sw:]
	def := strings.Index(body, "default:")
	if def < 0 {
		t.Fatal("the write-plan switch has no default arm, so an outcome nobody has " +
			"handled falls through to execution")
	}
	run := strings.Index(body, "Spec.Handler(")
	if run >= 0 && def > run {
		t.Error("the default arm comes after the handler call, so it cannot prevent execution")
	}
	if !strings.Contains(body[def:min(def+400, len(body))], "codeGovernedRefused") {
		t.Error("the default arm does not refuse. An unhandled outcome must not execute; " +
			"it must be turned away until someone decides what it means.")
	}
}

// An approval-required call must return WITHOUT executing, and must not report an
// error.
//
// Both halves matter. Executing would defeat the approval entirely. Reporting an error
// would invite a client to retry immediately or abandon the call, when the correct
// behaviour is to come back later and find out what the human decided — so the deferral
// is a success-shaped response carrying the pending id and a Retry-After.
func TestApprovalRequiredReturnsWithoutExecuting(t *testing.T) {
	src := governedSource(t)

	fn := strings.Index(src, "func handleApprovalRequired")
	if fn < 0 {
		t.Fatal("handleApprovalRequired is gone; approval-required calls have nowhere to go")
	}
	body := src[fn:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "Spec.Handler(") {
		t.Error("the approval-required path invokes the handler, which executes the very " +
			"write it is supposed to be holding for a person")
	}
	if !strings.Contains(body, "pending_action_id") {
		t.Error("the deferral does not return a pending action id, so nobody can be " +
			"pointed at the card that is waiting for them")
	}
	if !strings.Contains(body, `"isError": false`) {
		t.Error("the deferral is reported as an error. A client should come back and check " +
			"the decision, not retry immediately or give up.")
	}
}

// governedSource returns governed.go with comments stripped, so an assertion cannot be
// satisfied by prose. This file discusses every identifier it checks for.
func governedSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("governed.go")
	if err != nil {
		t.Fatalf("read governed.go: %v", err)
	}
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The audit record must be written before the handler runs, for the same reason.
//
// A decision that was made and not recorded is worse than one recorded and then
// abandoned, because only the second is discoverable afterwards. If the process dies
// mid-call, the record of what was authorised has to already exist.
func TestTheDecisionIsRecordedBeforeExecution(t *testing.T) {
	raw, err := os.ReadFile("governed.go")
	if err != nil {
		t.Fatalf("read governed.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	record := strings.Index(src, "RecordDecision(")
	if record < 0 {
		t.Fatal("the governed path no longer records its decision; refusals would leave " +
			"no trace, which is the gap this path was built to close")
	}
	if run := strings.Index(src, "Spec.Handler("); run >= 0 && record > run {
		t.Error("the decision is recorded after the handler runs, so a crash mid-call " +
			"leaves a change that happened with no record that it was authorised")
	}
}
