package botpost

import (
	"strings"
	"testing"
)

// The cases that matter are the ones where a diff block is NOT usable, because
// those are what a half-streamed reply looks like, and rendering a broken node
// mid-stream is worse than rendering the text.

func TestDiffFenceRendersClosedBlock(t *testing.T) {
	in := "Here is the fix:\n\n```diff\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n```\n"
	got := streamHTML(in, false)
	if !strings.Contains(got, `data-type="diff"`) {
		t.Fatalf("closed diff block did not become a diff node:\n%s", got)
	}
	if !strings.Contains(got, "Here is the fix") {
		t.Error("surrounding text was dropped")
	}
	// The patch must arrive escaped, never as live markup.
	if strings.Contains(got, "<script") {
		t.Error("unescaped content reached the output")
	}
}

func TestDiffFenceLeavesUnclosedBlockAsText(t *testing.T) {
	in := "```diff\n-old\n+new\n" // still streaming, no closing fence
	got := streamHTML(in, false)
	if strings.Contains(got, `data-type="diff"`) {
		t.Error("an unclosed block became a node; a partial stream would flash a broken diff")
	}
}

func TestDiffFenceRejectsNonDiffBody(t *testing.T) {
	in := "```diff\njust some prose with no patch lines\n```\n"
	got := streamHTML(in, false)
	if strings.Contains(got, `data-type="diff"`) {
		t.Error("a body with no patch lines became a diff node, which renders an empty review pane")
	}
}

func TestDiffFenceRejectsOversizePatch(t *testing.T) {
	body := "@@ -1 +1 @@\n" + strings.Repeat("+x\n", maxDiffAttrBytes)
	if _, ok := diffDivHTML(body); ok {
		t.Errorf("a patch over %d bytes was accepted into an attribute", maxDiffAttrBytes)
	}
}

func TestDiffFenceEscapesAttribute(t *testing.T) {
	got, ok := diffDivHTML(`@@ -1 +1 @@` + "\n" + `+<img src=x onerror="alert(1)">`)
	if !ok {
		t.Fatal("valid patch rejected")
	}
	if strings.Contains(got, "<img") {
		t.Errorf("markup survived into the attribute: %s", got)
	}
}

// The marker scan is shared with charts now, so a keyword must not match a
// longer word that merely starts with it.
func TestFenceMarkerRespectsWordBoundary(t *testing.T) {
	if fenceMarkerLen("```different", 0, "diff") != 0 {
		t.Error("```different matched the diff fence")
	}
	if fenceMarkerLen("```charter", 0, "chart") != 0 {
		t.Error("```charter matched the chart fence")
	}
	if fenceMarkerLen("``` Diff", 0, "diff") == 0 {
		t.Error("``` Diff should match: models write it that way")
	}
}

// Charts and diffs have to coexist in one reply.
func TestChartAndDiffInOneReply(t *testing.T) {
	in := "before\n\n```chart\n{\"type\":\"bar\",\"data\":[1]}\n```\n\nmiddle\n\n```diff\n@@ -1 +1 @@\n-a\n+b\n```\n\nafter"
	got := streamHTML(in, false)
	for _, want := range []string{`data-type="chart"`, `data-type="diff"`, "before", "middle", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
