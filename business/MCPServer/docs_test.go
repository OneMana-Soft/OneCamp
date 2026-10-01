package business

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
)

const mcpDocPath = "../../docs/MCPServer.md"

func mcpDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(mcpDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpDocPath, err)
	}
	return string(raw)
}

// WHY A DOCUMENT IS RATCHETED LIKE CODE.
//
// This one tells a customer which tools are governed, what the call budgets are, and what
// their agent can reach. They make security decisions from it — which tool groups to
// enable, which scopes to grant, whether a read-only agent is really read-only. A stale
// sentence here is not a typo; it is a customer believing a control exists that does not,
// which is the exact failure mode every ratchet in this package was written for.
//
// Documentation drifts silently because nothing compiles it. So the claims that can be
// checked against the code are checked here.
func TestTheMCPDocNamesExactlyTheGovernedTools(t *testing.T) {
	doc := mcpDoc(t)

	governed := map[string]bool{}
	for _, b := range bridgedTools {
		governed[b.Tool] = true
	}

	// The doc's governed table lists tools in `backticks`. Collect every backticked
	// token that is a real public tool name, so prose mentioning a tool in passing does
	// not confuse the comparison.
	mentioned := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(doc, -1) {
		name := m[1]
		if _, public := apiTokenBusiness.ScopeForTool(name); public {
			mentioned[name] = true
		}
	}

	for tool := range governed {
		if !mentioned[tool] {
			t.Errorf("%q is governed but the doc never names it. A customer reading the "+
				"governed table would not know this tool gets a per-object authority "+
				"check.", tool)
		}
	}

	// And every public tool must appear somewhere, governed or in the not-yet table:
	// a tool a customer can call and cannot find documented is one they cannot reason
	// about.
	for tool := range apiTokenBusiness.ToolScope {
		if !mentioned[tool] {
			t.Errorf("public tool %q is callable over /v1/mcp but appears nowhere in the "+
				"doc, so nothing tells a customer it exists or what governs it", tool)
		}
	}
}

// The doc states how many tools are governed. That number is the headline claim of the
// whole page and goes stale the moment a tool is added.
func TestTheMCPDocToolCountsAreCurrent(t *testing.T) {
	doc := mcpDoc(t)

	governedCount := len(bridgedTools)
	publicCount := len(apiTokenBusiness.ToolScope)
	ungovernedCount := publicCount - governedCount

	if want := fmt.Sprintf("%d of the %d public tools", governedCount, publicCount); !strings.Contains(doc, want) {
		t.Errorf("the doc does not say %q. It currently claims something else, so a reader "+
			"is told the wrong size of the governed surface.", want)
	}

	// The "not yet governed" section is titled with its own count — and must DISAPPEAR
	// when there is nothing left to list. A section headed "the none not yet governed"
	// would be the ratchet's arithmetic leaking into a customer-facing page, and a
	// section left in place with an empty table would suggest a gap that has closed.
	if ungovernedCount == 0 {
		if strings.Contains(doc, "not yet governed") {
			t.Error("every public tool is governed, but the doc still has a 'not yet " +
				"governed' section. Remove it — leaving it implies a gap that no longer " +
				"exists, and a customer reads that section to decide what to keep " +
				"unreachable.")
		}
		return
	}
	if want := fmt.Sprintf("The %s not yet governed", numberWord(ungovernedCount)); !strings.Contains(doc, want) {
		t.Errorf("the doc's ungoverned section is not titled %q; there are %d ungoverned "+
			"public tools", want, ungovernedCount)
	}
}

// The quoted call budgets must be the enforced ones. A customer sizing a client's retry
// behaviour against a number we no longer honour will build in a failure.
func TestTheMCPDocQuotesTheRealCallBudgets(t *testing.T) {
	doc := mcpDoc(t)

	for _, c := range []struct {
		label string
		value int
	}{
		{"read", DefaultReadCallsPerMinute},
		{"write", DefaultWriteCallsPerMinute},
	} {
		if !strings.Contains(doc, fmt.Sprintf("%d / minute", c.value)) {
			t.Errorf("the doc does not quote the enforced %s call budget of %d / minute",
				c.label, c.value)
		}
	}
}

// Scopes are what a customer picks when creating a token. An undocumented scope is one
// nobody grants, and a documented scope that no longer exists is one they cannot find.
func TestTheMCPDocDocumentsEveryScope(t *testing.T) {
	doc := mcpDoc(t)

	for _, scope := range apiTokenBusiness.AllScopes {
		if !strings.Contains(doc, "`"+scope+"`") {
			t.Errorf("scope %q is grantable but undocumented, so a customer choosing "+
				"scopes has no way to know what it permits", scope)
		}
	}

	// Every tool group must be named too, since the group list is the admission control
	// an admin actually operates.
	for _, group := range AllToolGroups() {
		if !strings.Contains(doc, "`"+group+"`") {
			t.Errorf("tool group %q can be enabled by an admin but is undocumented", group)
		}
	}
}

// The doc claims no tool currently triggers an approval. True today because every
// governed write is additive — and the day a destructive one is added, that sentence
// becomes false and misleads a customer into thinking a human is in the loop.
func TestTheMCPDocApprovalClaimMatchesTheRegistry(t *testing.T) {
	doc := mcpDoc(t)

	destructive := []string{}
	for _, b := range bridgedTools {
		if b.Behaviour.Destructive {
			destructive = append(destructive, b.Tool)
		}
	}

	claimsNone := strings.Contains(doc, "**No tool currently triggers this.**")
	if len(destructive) > 0 && claimsNone {
		t.Errorf("the doc says no tool triggers an approval, but these are declared "+
			"destructive and will: %v. Update the approvals section — a customer reading "+
			"it would not expect anyone to be asked.", destructive)
	}
	if len(destructive) == 0 && !claimsNone {
		t.Error("no governed tool is destructive, so nothing triggers an approval, but " +
			"the doc no longer says so. Claiming a human is asked when none is would be " +
			"the more dangerous direction of this error.")
	}
}

// numberWord renders small counts the way the doc's prose does.
func numberWord(n int) string {
	words := map[int]string{
		0: "none", 1: "one", 2: "two", 3: "three", 4: "four", 5: "five",
		6: "six", 7: "seven", 8: "eight", 9: "nine", 10: "ten",
		11: "eleven", 12: "twelve",
	}
	if w, ok := words[n]; ok {
		return w
	}
	return fmt.Sprintf("%d", n)
}

// The doc must quote the protocol revision the endpoint actually answers.
//
// A checkable claim that was not being checked. The document has always stated a version in prose,
// the handshake answered a private const in controllers/MCP, and the admin UI now prints one beside
// the endpoint it tells operators to connect to. Three copies, none of which would notice another
// changing — and the failure is quiet in the worst way: a client that trusts the documented revision
// and negotiates against a different one either fails for reasons that look like a network problem,
// or worse, appears to work while assuming semantics this endpoint does not implement.
//
// ProtocolVersion is exported for this reason, so the assertion compares against the value the
// server serves rather than a second literal written here.
func TestTheMCPDocQuotesTheServedProtocolVersion(t *testing.T) {
	doc := mcpDoc(t)

	if !strings.Contains(doc, ProtocolVersion) {
		t.Errorf("the doc never mentions %s, which is the revision controllers/MCP answers in "+
			"initialize. A reader configuring a client from this document would negotiate a "+
			"different one.", ProtocolVersion)
	}

	// And it must not still be advertising a revision we no longer answer. Only versions that are
	// real MCP identifiers are worth checking; anything else is prose.
	supported := map[string]bool{}
	for _, v := range SupportedProtocolVersions {
		supported[v] = true
	}
	for _, stale := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"} {
		if supported[stale] {
			continue
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(stale) + `\b`).MatchString(doc) {
			t.Errorf("the doc mentions protocol %s but this endpoint answers %s. One of the two is "+
				"wrong, and a customer cannot tell which.", stale, strings.Join(SupportedProtocolVersions, ", "))
		}
	}
}
