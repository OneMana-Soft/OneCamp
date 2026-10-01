package business

// The drift ratchet.
//
// This package exists because two independent surfaces must answer "may this
// identity authorize work" identically:
//
//   - business/MCPServer   — an external agent presenting a human's api_token.
//   - business/AIAgent     — the human at the root of an in-workspace delegation
//     chain.
//
// They are the same question asked through different doors. An identity refused at
// one must not be able to walk through the other: refusing a deactivated employee's
// MCP call while still running their scheduled agents would be a gap that looks
// closed from whichever side you audit first.
//
// Nothing in the type system enforces that both call Assess. A future author can
// add a third surface, or quietly drop the call from one of these two while fixing
// something nearby, and every existing test would still pass — because the gap only
// shows up as access that should have been refused, which no passing test asserts
// the absence of. So the guard is a test that reads its own callers.
//
// Checked from HERE rather than from each consumer on purpose. A consumer-side test
// is written by whoever adds the consumer, which is exactly the person who might
// forget. The invariant belongs to the package that defines it.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// consumers maps each file that must consult the gate to the specific access it
// would hand out if it stopped.
var consumers = map[string]string{
	"../MCPServer/reach.go": "an external agent could act on a deactivated person's " +
		"api_token, so offboarding would not revoke agent access",
	"../AIAgent/agentDelegation.go": "a delegation chain could stay rooted at a " +
		"deactivated person or a bot, so their scheduled and ambient agents would keep " +
		"running against surfaces they can no longer open themselves",
}

func TestBothAuthorizationSurfacesConsultTheEligibilityGate(t *testing.T) {
	for path, consequence := range consumers {
		t.Run(strings.TrimPrefix(path, "../"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v; if this file moved, move the gate with it", path, err)
			}
			// Strip comments so a mention in prose cannot satisfy the assertion —
			// both of these files discuss the gate at length.
			src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

			if !strings.Contains(src, "principalBusiness.Assess(") {
				t.Errorf("%s no longer calls principalBusiness.Assess. Consequence: %s", path, consequence)
			}
		})
	}
}

// Assess must stay free of I/O.
//
// Two reasons, and both are load-bearing. It is called on the authorization hot
// path of every MCP call and every delegated hop, where an extra round-trip is an
// extra dependency that can fail — and a gate that can fail independently of the
// lookup it guards needs its own fail-closed handling, which is how a guard grows
// the ability to crash the thing it protects. It also means every branch is
// testable exhaustively without a database, which is why the table in
// principal_test.go can cover cases that would otherwise need fixture users.
func TestAssessPerformsNoIO(t *testing.T) {
	raw, err := os.ReadFile("principal.go")
	if err != nil {
		t.Fatalf("read principal.go: %v", err)
	}
	src := string(raw)

	// A pure function needs neither of these. Their appearance is the earliest
	// signal that a lookup has been added.
	for _, forbidden := range []string{"context.Context", "domain/"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("principal.go references %q, which suggests Assess now performs a "+
				"lookup. Keep it pure: callers already fetch the user, the flags it needs "+
				"are already selected by that query, and a pure gate cannot fail separately "+
				"from the lookup it guards", forbidden)
		}
	}
}
