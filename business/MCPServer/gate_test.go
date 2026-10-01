package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func groupSpec(scope string) *ToolSpec {
	return &ToolSpec{Name: "t", RequiredScope: scope}
}

// Groups are derived from scopes, so a new tool joins an approved group automatically and
// cannot invent one nobody agreed to.
func TestToolGroupIsDerivedFromTheScope(t *testing.T) {
	for _, c := range []struct{ scope, want string }{
		{"tasks:read", "tasks"},
		{"tasks:write", "tasks"},
		{"messages:read", "messages"},
		{"data_sources:read", "data_sources"},
		{"  docs:write  ", "docs"},
		// No colon: its own group. Keeps the function total rather than returning an
		// error a caller cannot act on.
		{"standalone", "standalone"},
	} {
		if got := ToolGroup(groupSpec(c.scope)); got != c.want {
			t.Errorf("ToolGroup(%q) = %q, want %q", c.scope, got, c.want)
		}
	}
	if got := ToolGroup(nil); got != "" {
		t.Errorf("ToolGroup(nil) = %q, want empty", got)
	}
}

// Deny by default, at every step. A workspace that has said nothing exposes nothing.
//
// The three refusals are separated on purpose: they need three different actions from
// whoever reads them, and collapsing them into "not permitted" would send an operator
// hunting for a setting they had already found.
func TestAdmissionDeniesUntilAnAdminSaysOtherwise(t *testing.T) {
	spec := groupSpec("tasks:read")

	for _, c := range []struct {
		name       string
		settings   AdmissionSettings
		wantReason string
	}{
		{
			"zero value denies", AdmissionSettings{},
			"not enabled",
		},
		{
			"enabled but no groups named", AdmissionSettings{Enabled: true},
			"exposes no tool groups yet",
		},
		{
			// Whitespace and empty entries must not become a permission. This is the
			// shape a half-edited setting takes.
			"enabled with a blank group list", AdmissionSettings{Enabled: true, ToolGroups: " , ,, "},
			"exposes no tool groups yet",
		},
		{
			"a different group enabled", AdmissionSettings{Enabled: true, ToolGroups: "docs,messages"},
			"tasks tool group is not enabled",
		},
		{
			// Groups without the flag must not work. Otherwise an admin who named groups
			// and left the toggle off would have an open surface.
			"groups named but surface off", AdmissionSettings{ToolGroups: "*"},
			"not enabled",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := CheckAdmission(c.settings, spec)
			if got.Allow {
				t.Fatalf("admitted when it should not have been: %s", got.Reason)
			}
			if !strings.Contains(got.Reason, c.wantReason) {
				t.Errorf("reason = %q, want it to mention %q so an operator knows which "+
					"setting to look at", got.Reason, c.wantReason)
			}
		})
	}
}

// And the allows, without which every refusal above could be passing because nothing is
// ever admitted.
func TestAdmissionAllowsAnEnabledGroup(t *testing.T) {
	spec := groupSpec("tasks:read")

	for _, c := range []struct {
		name   string
		groups string
	}{
		{"exact group", "tasks"},
		{"among others", "docs,tasks,messages"},
		{"wildcard", "*"},
		{"case and space insensitive", "  TASKS  "},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := CheckAdmission(AdmissionSettings{Enabled: true, ToolGroups: c.groups}, spec); !got.Allow {
				t.Fatalf("group list %q did not admit a tasks tool: %s", c.groups, got.Reason)
			}
		})
	}
}

// A nil spec must not be admitted even with everything on. An admission check that
// answers yes about no tool is a check that can be satisfied by asking it nothing.
func TestAdmissionRefusesANilSpec(t *testing.T) {
	if got := CheckAdmission(AdmissionSettings{Enabled: true, ToolGroups: "*"}, nil); got.Allow {
		t.Fatal("a nil spec was admitted")
	}
}

// The catalogue must be narrowed by the same rule as the call.
// Asserted against CheckAdmission directly now that the AdmittedTools slice-filter is gone. The
// RULE is what matters and it has not changed; the shipped catalogue applies it per tool in
// listToolsForScopes, and TestServingEndpointAppliesTheSurfaceGates pins that it still does.
func TestAdmissionNarrowsTheCatalogueByTheSameRuleAsTheCall(t *testing.T) {
	enabled := AdmissionSettings{Enabled: true, ToolGroups: "tasks,docs"}

	admitted := 0
	for _, spec := range []*ToolSpec{
		groupSpec("tasks:read"),
		groupSpec("docs:read"),
		groupSpec("messages:read"),
	} {
		if CheckAdmission(enabled, spec).Allow {
			admitted++
		}
		// A disabled surface admits nothing, whatever the groups say.
		if CheckAdmission(AdmissionSettings{}, spec).Allow {
			t.Fatalf("%s was admitted while the surface is disabled", spec.Name)
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d tools, want 2; a tool whose group is not enabled must not be "+
			"listed, or an agent will try it and a tool name will disclose a capability an "+
			"admin chose not to expose", admitted)
	}
}

// The groups offered to an admin must come from the registry, so the choices are exactly
// the groups that exist.
func TestAllToolGroupsComesFromTheRegistry(t *testing.T) {
	if err := RegisterBridgedTools(); err != nil {
		// Already registered by another test in this package; either way the registry is
		// populated, which is what this asserts against.
		_ = err
	}

	groups := AllToolGroups()
	if len(groups) == 0 {
		t.Fatal("no tool groups derived from a populated registry")
	}
	seen := map[string]bool{}
	for _, g := range groups {
		if seen[g] {
			t.Errorf("group %q appears twice; an admin would see two identical choices", g)
		}
		seen[g] = true
		if g != strings.ToLower(strings.TrimSpace(g)) {
			t.Errorf("group %q is not a clean lowercase token, but is compared against a "+
				"stored allowlist verbatim", g)
		}
	}
	// Sorted, so the admin UI is stable between loads rather than reordering on every
	// map iteration.
	for i := 1; i < len(groups); i++ {
		if groups[i-1] > groups[i] {
			t.Fatalf("groups are not sorted (%q before %q); the admin list would reorder "+
				"between page loads", groups[i-1], groups[i])
		}
	}
}

// Admission must be checked BEFORE the scope check, and before any permission lookup.
//
// It is the broader gate: if the surface is off, nothing about the credential can change
// the answer. Checking it later would spend graph queries on a call a disabled workspace
// was always going to refuse — and would mean the reason returned depends on which check
// happened to run first.
func TestAdmissionIsCheckedBeforeScopeAndReach(t *testing.T) {
	raw, err := os.ReadFile("authorize.go")
	if err != nil {
		t.Fatalf("read authorize.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	admission := strings.Index(src, "CheckAdmission(")
	if admission < 0 {
		t.Fatal("the ladder no longer checks admission. Every registered tool would be " +
			"reachable by any token holding its scope, with no way for an admin to say " +
			"whether this surface is exposed at all.")
	}
	for _, later := range []string{"hasScope(", "PrincipalCanReach(", "CheckBudget("} {
		i := strings.Index(src, later)
		if i >= 0 && i < admission {
			t.Errorf("%s runs before CheckAdmission; a workspace that has not enabled this "+
				"surface should not reach any per-credential or per-object work", later)
		}
	}
}

// The settings read must fail CLOSED.
//
// If the admin's decision cannot be read, it is unknown — and the only safe reading of an
// unknown admission decision is that the surface is closed. Failing open here would mean
// a database blip silently exposes every tool group.
func TestAnUnreadableSettingRefusesRatherThanAdmits(t *testing.T) {
	raw, err := os.ReadFile("authorize.go")
	if err != nil {
		t.Fatalf("read authorize.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	get := strings.Index(src, "aiSettingsModel.GetSettings(")
	if get < 0 {
		t.Fatal("the ladder no longer loads the workspace's MCP settings")
	}
	window := src[get:min(get+300, len(src))]
	if !strings.Contains(window, "denyAs(") {
		t.Error("a settings read error does not lead to a refusal. An unknown admission " +
			"decision must close the surface, not open it.")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
