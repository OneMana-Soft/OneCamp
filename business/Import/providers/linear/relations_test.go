package linear

import (
	"strings"
	"testing"
)

// L-2: Linear keeps each link once, on the issue it points at (its inverse
// relations). A duplicate says what it duplicates and the original lists its
// duplicates; related issues list each other; a link to an issue that came too
// opens its task here by key, and one that didn't opens it in Linear. "blocks"
// stays a dependency, and "similar" (Linear's own guess) isn't brought.
func TestDuplicateAndRelatedLinksComeAsLines(t *testing.T) {
	issues := []linearIssue{
		{ID: "x", Identifier: "ENG-1", URL: "https://linear.app/a/issue/ENG-1"},
		{ID: "a", Identifier: "ENG-2", URL: "https://linear.app/a/issue/ENG-2"},
		{ID: "b", Identifier: "ENG-3", URL: "https://linear.app/a/issue/ENG-3"},
		{ID: "e", Identifier: "ENG-5"},
	}
	for _, rel := range []rawRelation{
		relation("duplicate", "a", "ENG-2", ""),
		relation("related", "b", "ENG-3", ""),
		relation("related", "far", "OPS-9", "https://linear.app/a/issue/OPS-9"),
		relation("related", "odd", "OPS-7", "javascript:alert(1)"),
		relation("similar", "b", "ENG-3", ""),
		relation("blocks", "e", "ENG-5", ""),
	} {
		issues[0].addRelation(rel)
	}
	byID := map[string]*linearIssue{}
	for i := range issues {
		byID[issues[i].ID] = &issues[i]
	}
	linkRelations(issues, byID)

	if len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0] != "e" {
		t.Errorf("blocked by %v, want e", issues[0].BlockedBy)
	}
	desc := func(i int) string { return New().issueToSourceTask(issues[i], "p").Description }
	for _, want := range []string{
		`Duplicates</strong> <a href="/app/task/ENG-2">ENG-2</a>`,
		// An address that isn't https is kept as the key alone.
		`Related</strong> <a href="/app/task/ENG-3">ENG-3</a>, <a href="https://linear.app/a/issue/OPS-9">OPS-9</a>, OPS-7</p>`,
	} {
		if !strings.Contains(desc(0), want) {
			t.Errorf("the original's description %q lacks %q", desc(0), want)
		}
	}
	if d := desc(1); !strings.Contains(d, `Duplicate of</strong> <a href="/app/task/ENG-1">ENG-1</a>`) {
		t.Errorf("the duplicate's description %q doesn't say what it duplicates", d)
	}
	if d := desc(2); !strings.Contains(d, `Related</strong> <a href="/app/task/ENG-1">ENG-1</a>`) || strings.Count(d, ">ENG-1<") != 1 {
		t.Errorf("the related issue's description %q should list ENG-1 once (similar isn't brought)", d)
	}
	if d := desc(3); strings.Contains(d, "Related") || strings.Contains(d, "Duplicate") {
		t.Errorf("a blocker's description %q gained a link line", d)
	}
}

func relation(typ, id, key, url string) rawRelation {
	r := rawRelation{Type: typ}
	r.Issue = &relatedIssue{ID: id, Identifier: key, URL: url}
	return r
}
