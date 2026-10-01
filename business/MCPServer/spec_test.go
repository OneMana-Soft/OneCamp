package business

// The registry is the enforcement point for two invariants that cannot be checked
// anywhere else:
//
//  1. every tool declares WHAT IT TOUCHES, so a per-resource authority check is
//     possible. Without this a tool runs on the strength of its scope alone, which
//     is precisely the confused-deputy path this package exists to close.
//  2. every tool has a description and schema, and they are server-controlled.
//     Tool catalogues are a documented prompt-injection vector — names,
//     descriptions and schemas enter a model's context as trusted instructions.
//
// These are the tests that make those invariants structural rather than habitual.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func validSpec(name string) *ToolSpec {
	return &ToolSpec{
		Name:          name,
		Description:   "A static description.",
		InputSchema:   json.RawMessage(`{"type":"object"}`),
		RequiredScope: "docs:read",
		Behaviour:     ToolBehaviour{ReadOnly: true},
		Resource: func(map[string]any) (ResourceRef, error) {
			return ResourceRef{Kind: ResourceWorkspace}, nil
		},
		Handler: func(context.Context, ToolCallContext) (any, error) { return nil, nil },
	}
}

func TestRegisterAcceptsAValidSpec(t *testing.T) {
	resetRegistryForTest()
	if err := Register(validSpec("good_tool")); err != nil {
		t.Fatalf("a valid spec was refused: %v", err)
	}
	if _, ok := Lookup("good_tool"); !ok {
		t.Fatal("registered tool is not retrievable")
	}
}

// The invariant that matters most. A tool with no Resource function cannot be
// authorised against anything.
func TestRegisterRefusesAToolThatDeclaresNoResource(t *testing.T) {
	resetRegistryForTest()
	spec := validSpec("ungoverned")
	spec.Resource = nil

	err := Register(spec)
	if err == nil {
		t.Fatal("a tool with no Resource function was accepted; it would run on its " +
			"scope alone, with no per-resource authority check — the confused-deputy path")
	}
	if !strings.Contains(err.Error(), "Resource") {
		t.Errorf("the error should name the missing Resource function, got: %v", err)
	}
}

func TestRegisterRefusesIncompleteSpecs(t *testing.T) {
	cases := map[string]func(*ToolSpec){
		"no name":           func(s *ToolSpec) { s.Name = "" },
		"blank name":        func(s *ToolSpec) { s.Name = "   " },
		"no description":    func(s *ToolSpec) { s.Description = "" },
		"no schema":         func(s *ToolSpec) { s.InputSchema = nil },
		"invalid schema":    func(s *ToolSpec) { s.InputSchema = json.RawMessage(`{not json`) },
		"no required scope": func(s *ToolSpec) { s.RequiredScope = "" },
		"blank scope":       func(s *ToolSpec) { s.RequiredScope = "  " },
		"no handler":        func(s *ToolSpec) { s.Handler = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			resetRegistryForTest()
			spec := validSpec("t")
			mutate(spec)
			if err := Register(spec); err == nil {
				t.Errorf("an incomplete spec (%s) was accepted", name)
			}
		})
	}
}

func TestRegisterRefusesNilAndDuplicates(t *testing.T) {
	resetRegistryForTest()
	if err := Register(nil); err == nil {
		t.Error("a nil spec was accepted")
	}
	if err := Register(validSpec("dup")); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := Register(validSpec("dup")); err == nil {
		t.Error("a duplicate tool name was accepted; the second would shadow the first")
	}
}

// The VisibleTools tests that were here — scope filtering and a sorted catalogue — went with the
// function, which existed only for protocol.go's removed ListTools. No coverage of a live rule
// was lost: the scope rule itself is hasScope, still used by both authorize ladders and pinned by
// TestHasScopeIsExactMatch below, and the shipped catalogue's scope filtering is pinned by
// TestServingEndpointAppliesTheSurfaceGates. The sort assertion had nothing left to describe —
// listToolsForScopes iterates ai.ToolRegistry in its own order.

// Scope matching is exact on purpose. A wildcard or prefix match is the kind of
// convenience where "channels:*" quietly grants "channels:delete".
func TestHasScopeIsExactMatch(t *testing.T) {
	granted := []string{"docs:read", "channels:read"}
	if !hasScope(granted, "docs:read") {
		t.Error("an exactly granted scope was not matched")
	}
	if !hasScope(granted, "DOCS:READ") {
		t.Error("scope matching should be case-insensitive")
	}
	for _, notGranted := range []string{"docs:write", "docs", "docs:*", "*", "", "  "} {
		if hasScope(granted, notGranted) {
			t.Errorf("scope %q must not be satisfied by %v", notGranted, granted)
		}
	}
}
