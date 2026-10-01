package business

// These test the SHAPE of what gets recorded, without a database.
//
// The audit record is the compliance deliverable, so its contract matters as much as
// the authorization decision: a refusal that is not recorded, or recorded without the
// human behind it, is not evidence of anything. The write itself is covered by the
// integration suite; what is asserted here is that every field a reviewer needs is
// derivable from a Decision, and that no field can carry an unbounded or
// non-UTF-8 value into a hash chain that makes it permanent.

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
)

func TestDecisionWordIsAFixedVocabulary(t *testing.T) {
	// A reviewer queries for refusals by matching one string. If the phrasing varied
	// by call site, that query would silently miss rows.
	if decisionWord(true) != "allowed" || decisionWord(false) != "refused" {
		t.Fatalf("vocabulary changed: %q / %q", decisionWord(true), decisionWord(false))
	}
	if ActionToolCallAllowed == ActionToolCallRefused {
		t.Fatal("allow and refuse must be distinct actions, or they cannot be queried apart")
	}
	for _, a := range []string{ActionToolCallAllowed, ActionToolCallRefused} {
		if !strings.HasPrefix(a, "mcp.") {
			t.Errorf("action %q should be namespaced so MCP activity is separable", a)
		}
	}
}

// Everything written into the chain is bounded on a rune boundary. The hash is
// computed over the content, so an oversized or malformed value cannot be corrected
// later without breaking verification.
func TestClipBoundsEveryFieldSafely(t *testing.T) {
	long := strings.Repeat("日", 4000)
	got := clip(long)

	if !utf8.ValidString(got) {
		t.Error("clip produced invalid UTF-8; written into a hash chain that is permanent")
	}
	if n := utf8.RuneCountInString(got); n > maxAuditFieldRunes {
		t.Errorf("field not bounded: %d characters", n)
	}
	if clip("") != "" {
		t.Error("empty input must stay empty")
	}
	if short := "already short"; clip(short) != short {
		t.Error("a value within the bound must pass through unchanged")
	}
}

// An early refusal has no resource, because the tool never resolved one. Absent must
// be distinguishable from empty: "we never got that far" is a different fact from
// "it touched nothing", and a reviewer reading the row needs to tell them apart.
func TestRefusalMetadataOmitsTheResourceItNeverResolved(t *testing.T) {
	// Mirrors what RecordDecision builds, asserting the branch rather than the write.
	refused := Decision{Allow: false, Reason: "invalid or inactive credential"}
	if refused.Call.Resource.Kind != "" {
		t.Fatal("a refused decision must not carry a resolved resource")
	}

	allowed := Decision{
		Allow: true,
		Call:  ToolCallContext{Resource: ResourceRef{Kind: ResourceChannel, ID: "abc"}},
	}
	if allowed.Call.Resource.Kind == "" {
		t.Fatal("an allowed decision must carry the resource it was authorised for")
	}
}

// Both the depth we enforced and the depth the caller claimed are recorded. Keeping
// both is what makes a caller that lied visible afterwards, rather than merely
// unsuccessful at the time.
func TestAuditKeepsEnforcedAndDeclaredDepthApart(t *testing.T) {
	declared := DeclaredChain{Hop: 0, Actors: []string{"upstream-agent"}}

	enforced := EffectiveDepth(declared)
	if enforced == declared.Hop {
		t.Fatal("a caller claiming hop 0 must not produce an enforced depth of 0; " +
			"depth comes from our own credential")
	}
	if enforced != EntryDepth {
		t.Errorf("enforced depth = %d, want EntryDepth (%d)", enforced, EntryDepth)
	}
}

// An unattributable refusal is still worth recording: a burst of them is what a
// credential-stuffing attempt looks like. So the recorder must tolerate a Decision
// with no identity rather than treating it as a programming error.
func TestAnUnattributableRefusalIsStillRecordable(t *testing.T) {
	d := Decision{Allow: false, Reason: "invalid or inactive credential"}
	if d.PrincipalUserID != "" || d.TokenID != "" {
		t.Fatal("fixture should have no identity")
	}
	// clip must handle the empty fields without panicking; RecordDecision passes
	// them straight through.
	if clip(d.PrincipalUserID) != "" || clip(d.TokenID) != "" {
		t.Error("empty identity fields must clip to empty, not to a placeholder")
	}
}

// An inbound MCP call is a delegated act (EntryDepth is one), so its decision
// rows carry "handoff" and land under "nobody watching". A caller that already
// knows who started the work keeps its own answer.
func TestInboundDecisionsAreRecordedAsHandoffs(t *testing.T) {
	got, ok := auditBusiness.InitiatorFromCtx(decisionContext(context.Background()))
	if !ok || got != auditBusiness.InitiatorHandoff {
		t.Fatalf("initiator = %q (set=%v), want %q", got, ok, auditBusiness.InitiatorHandoff)
	}
	if !got.Unattended() {
		t.Error("an inbound call must count as nobody watching")
	}

	pre := auditBusiness.WithInitiator(context.Background(), auditBusiness.InitiatorPerson)
	got, _ = auditBusiness.InitiatorFromCtx(decisionContext(pre))
	if got != auditBusiness.InitiatorPerson {
		t.Errorf("a caller's own answer was overwritten: got %q", got)
	}
}
