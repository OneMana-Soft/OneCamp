package business

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The digest identifies a call without keeping what it carried.
func TestParamsDigestIsStableAndContentFree(t *testing.T) {
	a := map[string]string{"channel": "eng", "body": "ship it"}
	b := map[string]string{"body": "ship it", "channel": "eng"}

	// Map iteration order is random in Go; the same call must digest the same
	// or two identical actions look like two different ones in the ledger.
	if paramsDigest(a) != paramsDigest(b) {
		t.Error("digest depends on map iteration order")
	}

	// Different content, different digest, so a substituted parameter is visible.
	if paramsDigest(a) == paramsDigest(map[string]string{"channel": "eng", "body": "do not ship"}) {
		t.Error("two different calls share a digest")
	}

	// The digest must not carry the content it fingerprints.
	d := paramsDigest(a)
	for _, secret := range []string{"ship it", "eng"} {
		if strings.Contains(d, secret) {
			t.Errorf("digest leaks %q", secret)
		}
	}
}

// Read-only calls are deliberately not recorded: a lookup that went unrecorded
// cannot mean an unrecorded change, and a synchronous write per read would cost
// something and prove nothing.
func TestReadOnlyCallsAreNotRecorded(t *testing.T) {
	// There is no database in this binary. A read-only tool must return before
	// touching one; anything else would panic or error here.
	id, err := recordActionIntent(context.Background(), nil, uuid.New(), nil, "list_tasks", map[string]string{})
	if err != nil {
		t.Fatalf("a read-only call attempted a write: %v", err)
	}
	if id != nil {
		t.Error("a read-only call produced an intent row")
	}
}

// FAIL CLOSED. An effecting action whose intent cannot be written must not run.
// If this ever returns a nil error the caller proceeds, and the completeness
// claim silently stops being true.
func TestAnEffectingCallRefusesWhenItCannotBeRecorded(t *testing.T) {
	// No database, so the insert cannot succeed.
	_, err := recordActionIntent(context.Background(), nil, uuid.New(), nil, "send_message", map[string]string{"body": "x"})
	if err == nil {
		t.Fatal("an effecting action was allowed to run without a durable record of it")
	}
	if !strings.Contains(err.Error(), "not run") {
		t.Errorf("the refusal does not say the action did not run: %q", err)
	}
}

// Closing an intent that was never opened is a no-op, so the read-only path
// cannot accidentally write an outcome for a row that does not exist.
func TestClosingANilIntentIsSafe(t *testing.T) {
	closeActionIntent(context.Background(), nil, "ok", "")
}

// THE ORDERING IS THE CLAIM. Recording after the attempt would leave exactly the
// gap this exists to close, and no unit test can observe a crash between the two,
// so the order is pinned at the source.
func TestIntentIsRecordedBeforeTheAttempt(t *testing.T) {
	src, err := os.ReadFile("agentRunner.go")
	if err != nil {
		t.Fatalf("read agentRunner.go: %v", err)
	}
	body := string(src)

	intent := strings.Index(body, "recordActionIntent(")
	if intent < 0 {
		t.Fatal("the runner does not record intent at all; effecting actions can happen unrecorded")
	}
	// The execution call this guards.
	attempt := strings.Index(body, "exec(loopCtx, a, userUUID)")
	if attempt < 0 {
		t.Fatal("could not find the tool execution site")
	}
	if intent > attempt {
		t.Error("intent is recorded AFTER the attempt, which is the gap this was built to close")
	}

	// And the refusal must stop the attempt.
	between := body[intent:attempt]
	if !strings.Contains(between, "ierr != nil") {
		t.Error("a failed intent write does not prevent the attempt")
	}
}

// An unrecognised tool must be treated as effecting, not waved through.
//
// ToolIsReadOnly returns false for anything it does not know, which is the
// fail-safe direction: a tool added later, or one an MCP server supplies, gets
// recorded rather than silently escaping the ledger.
func TestAnUnknownToolIsTreatedAsEffecting(t *testing.T) {
	_, err := recordActionIntent(context.Background(), nil, uuid.New(), nil,
		"some_tool_nobody_registered", map[string]string{"x": "1"})
	if err == nil {
		t.Fatal("an unknown tool bypassed the record-before-acting requirement")
	}
}
