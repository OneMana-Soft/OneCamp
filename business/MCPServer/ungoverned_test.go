package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
)

// stripComments removes comments so a source-order assertion cannot be satisfied or
// broken by prose that merely mentions a function name.
func stripComments(src string) string {
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(src, " ")
}

func mustReadSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return stripComments(string(raw))
}

// THE ANTI-DRIFT RATCHET. Two ladders exist because they end in different guarantees,
// but the SURFACE rungs they share must stay in the same order — and above all, the
// consuming one must stay last on both. Two orderings maintained by hand is exactly
// how one of them ends up charging a budget before checking a scope.
func TestBothLaddersRunTheSameSurfaceRungsInTheSameOrder(t *testing.T) {
	// Rung -> the needle that proves it ran, per ladder.
	rungs := []struct {
		name      string
		governed  string
		ungovrned string
	}{
		{"credential", "apiTokenBusiness.Validate(", "apiTokenBusiness.Validate("},
		{"agent kill switch", "ResolveActor(", "ResolveActor("},
		{"agent toolset", "CheckAgentToolScope(", "CheckAgentToolScope("},
		{"admission", "CheckAdmission(", "CheckAdmissionForScope("},
		{"scope", "hasScope(", "hasScope("},
		{"budget (must be last)", "CheckBudget(", "CheckCallBudget("},
	}

	for _, ladder := range []struct {
		label  string
		file   string
		needle func(int) string
	}{
		{"governed", "authorize.go", func(i int) string { return rungs[i].governed }},
		{"ungoverned", "ungoverned.go", func(i int) string { return rungs[i].ungovrned }},
	} {
		t.Run(ladder.label, func(t *testing.T) {
			src := mustReadSource(t, ladder.file)
			prev := -1
			prevName := ""
			for i := range rungs {
				at := strings.Index(src, ladder.needle(i))
				if at < 0 {
					t.Fatalf("the %s ladder no longer runs the %q rung (looked for %q). "+
						"Every surface gate must apply on both paths, or a tool is covered "+
						"on one and exposed on the other.",
						ladder.label, rungs[i].name, ladder.needle(i))
				}
				if prev >= 0 && at < prev {
					t.Errorf("the %s ladder runs %q BEFORE %q; the shared order is "+
						"cheapest-and-most-absolute first, with the consuming check last",
						ladder.label, rungs[i].name, prevName)
				}
				prev = at
				prevName = rungs[i].name
			}
		})
	}
}

// A refusal on the ungoverned path must be attributable. A refusal nobody can attribute
// is a refusal nobody can investigate, which is most of the value of recording it.
func TestUngovernedRefusalsCarryTheIdentityTheyKnow(t *testing.T) {
	src := mustReadSource(t, "ungoverned.go")

	// Every refusal after the credential is resolved returns `base`, which carries the
	// token id, principal and actor. Only the pre-credential refusal may not.
	returns := regexp.MustCompile(`return UngovernedDecision\{[^}]*\}`).FindAllString(src, -1)
	if len(returns) == 0 {
		t.Fatal("no literal UngovernedDecision returns found; this ratchet has gone stale")
	}
	for _, r := range returns {
		if strings.Contains(r, "invalid or inactive credential") {
			continue // nothing is known yet, by construction
		}
		t.Errorf("a refusal is built as a bare literal and so drops the resolved "+
			"identity: %s\n  -> populate and return `base` instead, so the audit row "+
			"names which agent was refused", strings.TrimSpace(r))
	}
}

// EVERY public tool's group must be offerable to an admin.
//
// This is the gap that made admission unenforceable for part of the catalogue: the
// choices were derived from the GOVERNED registry, but the tools being gated come from
// the full public catalogue. calendar and data_sources had no governed tool, so their
// groups were never offered — and gating those tools on a group nobody can enable
// makes them permanently unreachable with no setting able to say otherwise.
func TestEveryPublicToolGroupIsOfferableToAnAdmin(t *testing.T) {
	offerable := map[string]bool{}
	for _, g := range AllToolGroups() {
		offerable[g] = true
	}

	for tool, scope := range apiTokenBusiness.ToolScope {
		group := ToolGroupForScope(scope)
		if group == "" {
			t.Errorf("public tool %q has scope %q which yields no group, so no admin "+
				"setting can admit it", tool, scope)
			continue
		}
		if !offerable[group] {
			t.Errorf("public tool %q is in group %q, which AllToolGroups does not offer: "+
				"an admin cannot enable it, so admission would refuse it forever",
				tool, group)
		}
	}
}

// Admission on a bare scope must deny by default in each of the three distinguishable
// ways, and must honour the wildcard.
func TestAdmissionForScopeDeniesByDefault(t *testing.T) {
	cases := []struct {
		name     string
		settings AdmissionSettings
		scope    string
		allow    bool
	}{
		{"surface off", AdmissionSettings{Enabled: false, ToolGroups: "*"}, "tasks:read", false},
		{"on, no groups", AdmissionSettings{Enabled: true, ToolGroups: ""}, "tasks:read", false},
		{"group not enabled", AdmissionSettings{Enabled: true, ToolGroups: "docs"}, "tasks:read", false},
		{"group enabled", AdmissionSettings{Enabled: true, ToolGroups: "docs,tasks"}, "tasks:read", true},
		{"wildcard", AdmissionSettings{Enabled: true, ToolGroups: "*"}, "calendar:write", true},
		{"no scope", AdmissionSettings{Enabled: true, ToolGroups: "*"}, "", false},
		{"whitespace and case", AdmissionSettings{Enabled: true, ToolGroups: " TASKS , "}, "tasks:read", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckAdmissionForScope(tc.settings, tc.scope)
			if got.Allow != tc.allow {
				t.Fatalf("allow=%v want %v (reason: %s)", got.Allow, tc.allow, got.Reason)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("reason is empty; the audit record and the client message both read it")
			}
		})
	}
}

// The spec-based and scope-based admission entry points must agree, or governing a
// tool would silently change whether the workspace exposes it.
func TestSpecAndScopeAdmissionAgree(t *testing.T) {
	settings := AdmissionSettings{Enabled: true, ToolGroups: "tasks"}
	spec := &ToolSpec{Name: "x", RequiredScope: "tasks:read"}

	viaSpec := CheckAdmission(settings, spec)
	viaScope := CheckAdmissionForScope(settings, spec.RequiredScope)

	if viaSpec.Allow != viaScope.Allow || viaSpec.Reason != viaScope.Reason {
		t.Fatalf("the two admission entry points disagree:\n  spec:  %+v\n  scope: %+v",
			viaSpec, viaScope)
	}
}

// A call with no credential to charge must be refused rather than run uncharged: an
// unattributable call is exactly what a budget cannot bound.
func TestCheckCallBudgetRefusesWithoutACredential(t *testing.T) {
	if d := CheckCallBudget(nil, "", true, BudgetLimits{}); d.Allow {
		t.Fatal("a call with no credential was allowed and charged to nothing")
	}
}

// CONTROLLER RATCHET. The serving endpoint must apply admission to the CATALOGUE as
// well as the call, run the ungoverned branch through the gate, and attribute the
// executor's AI spend. Each of these looks fine when missing — the tool works, the
// answer is right — and only a control quietly stops holding.
func TestServingEndpointAppliesTheSurfaceGates(t *testing.T) {
	src := mustReadSource(t, "../../controllers/MCP/mcpServerController.go")

	for _, want := range []struct {
		needle string
		why    string
	}{
		{"AuthorizeUngovernedCall(",
			"ungoverned tools would skip admission, the agent kill switch and the call budget"},
		{"SpendContext(",
			"the executor would run unattributed, so the per-agent daily token cap could not engage"},
		{"CheckAdmissionForScope(",
			"tools/list would advertise tools a disabled workspace refuses, and disclose " +
				"capabilities an admin chose not to expose"},
		{"auditBusiness.Record(",
			"outcomes on this path would leave no record"},
		{"AnnotationsFor(",
			"tools/list would advertise no readOnlyHint or destructiveHint, leaving a client " +
				"with only two options and both wrong: prompt a person for every read, or run " +
				"every write without asking"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Errorf("the MCP serving endpoint no longer calls %s: %s", want.needle, want.why)
		}
	}

	// The executor must NOT receive the bare request context, which is the exact
	// regression SpendContext exists to prevent.
	if regexp.MustCompile(`executor\(\s*r\.Context\(\)`).MatchString(src) {
		t.Error("the executor runs on the bare request context; wrap it in " +
			"mcpBusiness.SpendContext(r.Context(), decision.Actor) so an agent-bound " +
			"credential is charged against the agent's own daily token cap")
	}

	// Admission must be decided before the scope filter reaches a tool description,
	// and the refusal must precede execution.
	authorize := strings.Index(src, "AuthorizeUngovernedCall(")
	exec := strings.Index(src, "executor(")
	if authorize < 0 || exec < 0 || authorize > exec {
		t.Error("the ungoverned tool executes before it is authorized")
	}
}
