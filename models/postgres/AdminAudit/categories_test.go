package models

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// AllCategories must list EVERY declared category.
//
// This is the ratchet for a failure that already happened once. CategoryAgent was
// added on its own, away from the other five, and the admin UI's hardcoded filter
// list never learned about it — so every agent and MCP audit entry, including every
// refusal, could only be seen under "all". A governance surface whose evidence
// cannot be selected is barely evidence.
//
// Now the server serves the list and the client renders it, so the only way to
// reintroduce the bug is to declare a category and leave it out of AllCategories.
// This makes that a build failure.
//
// Read from source rather than maintained by hand, because a hand-maintained list
// of the things a hand-maintained list might miss is not a check.
func TestAllCategoriesListsEveryDeclaredCategory(t *testing.T) {
	raw, err := os.ReadFile("adminAuditModel.go")
	if err != nil {
		t.Fatalf("read adminAuditModel.go: %v", err)
	}
	src := string(raw)

	// Every `CategoryX = "value"` constant in this file.
	declared := regexp.MustCompile(`(?m)^\s*(Category\w+)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
	if len(declared) == 0 {
		t.Fatal("found no Category constants; this test has gone stale")
	}

	listed := map[string]bool{}
	for _, c := range AllCategories() {
		listed[c] = true
	}

	for _, m := range declared {
		name, value := m[1], m[2]
		if !listed[value] {
			t.Errorf("%s (%q) is declared but missing from AllCategories(), so the admin "+
				"audit UI will not offer a filter for it and every entry recorded under it "+
				"will be visible only under \"all\". This is the exact bug CategoryAgent "+
				"had.", name, value)
		}
	}

	// And the other direction: AllCategories must not invent a value that no
	// constant declares, or the UI would offer a filter that matches nothing.
	declaredValues := map[string]bool{}
	for _, m := range declared {
		declaredValues[m[2]] = true
	}
	for _, c := range AllCategories() {
		if !declaredValues[c] {
			t.Errorf("AllCategories() includes %q, which no Category constant declares; the "+
				"UI would show a filter that can never match an entry", c)
		}
	}
}

// Duplicates would render two identical filter buttons.
func TestAllCategoriesHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range AllCategories() {
		if seen[c] {
			t.Errorf("category %q appears twice in AllCategories()", c)
		}
		seen[c] = true
	}
}

// Values must be safe to put in a URL query and compare exactly: the client sends
// one back as ?category=, and the store filters on it verbatim.
func TestCategoryValuesAreSimpleTokens(t *testing.T) {
	token := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, c := range AllCategories() {
		if !token.MatchString(c) {
			t.Errorf("category %q is not a simple lowercase token; it round-trips through a "+
				"URL query parameter and is compared verbatim", c)
		}
		if strings.TrimSpace(c) != c {
			t.Errorf("category %q has surrounding whitespace", c)
		}
	}
}
