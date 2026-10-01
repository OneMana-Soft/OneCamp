package business

import (
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// TestToolScopeAlignment guards the invariant that every publicly-exposed tool
// maps to a known scope AND names a real tool in the AI registry, so the MCP
// server / REST surface can never advertise a tool that doesn't exist or gate
// it behind an ungrantable scope.
func TestToolScopeAlignment(t *testing.T) {
	known := map[string]bool{}
	for _, s := range AllScopes {
		known[s] = true
	}
	registry := map[string]bool{}
	for _, tdef := range ai.ToolRegistry {
		registry[tdef.Name] = true
	}
	for tool, scope := range ToolScope {
		if !known[scope] {
			t.Errorf("tool %q maps to scope %q which is not in AllScopes", tool, scope)
		}
		if !registry[tool] {
			t.Errorf("tool %q in ToolScope is not a registered AI tool", tool)
		}
	}
}

func TestSearchWorkspaceExposed(t *testing.T) {
	scope, public := ScopeForTool("search_workspace")
	if !public || scope != ScopeSearchRead {
		t.Fatalf("search_workspace should be public under %q, got (%q, %v)", ScopeSearchRead, scope, public)
	}
	found := false
	for _, s := range AllScopes {
		if s == ScopeSearchRead {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q must be in AllScopes so it is grantable", ScopeSearchRead)
	}
}
