package business

import (
	"strings"
	"testing"
)

func TestFlattenCitations(t *testing.T) {
	groups := []UnifiedSearchGroup{
		{Source: UnifiedSourceWorkspace, Hits: []UnifiedHit{
			{Source: UnifiedSourceWorkspace, Title: "Post by Alice", Snippet: "shipped the fix", PostUUID: "p1"},
			{Source: UnifiedSourceWorkspace, Title: "", Snippet: ""}, // skipped: empty
		}},
		{Source: UnifiedSourceMemory, Hits: []UnifiedHit{
			{Source: UnifiedSourceMemory, Title: "Decision: use Postgres", Kind: "decision"},
		}},
		{Source: UnifiedSourceGitHub, Hits: []UnifiedHit{
			{Source: UnifiedSourceGitHub, Title: "Fix login", URL: "https://github.com/o/r/pull/1"},
		}},
	}
	cs := flattenCitations(groups, 8)
	if len(cs) != 3 {
		t.Fatalf("want 3 citations, got %d", len(cs))
	}
	if cs[0].Index != 1 || cs[1].Index != 2 || cs[2].Index != 3 {
		t.Fatalf("indexes not 1-based sequential: %+v", cs)
	}
	if cs[0].PostUUID != "p1" {
		t.Fatalf("routing not carried forward: %+v", cs[0])
	}
	if cs[2].URL == "" {
		t.Fatalf("external URL not carried forward: %+v", cs[2])
	}
}

func TestFlattenCitationsCap(t *testing.T) {
	var hits []UnifiedHit
	for i := 0; i < 20; i++ {
		hits = append(hits, UnifiedHit{Source: UnifiedSourceWorkspace, Title: "t"})
	}
	cs := flattenCitations([]UnifiedSearchGroup{{Source: UnifiedSourceWorkspace, Hits: hits}}, 5)
	if len(cs) != 5 {
		t.Fatalf("cap not applied: got %d", len(cs))
	}
}

func TestReferencedIndexes(t *testing.T) {
	cases := []struct {
		text string
		want []int
	}{
		{"The fix shipped [1] and was reviewed [2][3].", []int{1, 2, 3}},
		{"Per the decision [2, 4], we use Postgres.", []int{2, 4}},
		{"No citations here.", nil},
		{"Weird [x] and empty [] tokens ignored, but [7] counts.", []int{7}},
	}
	for _, c := range cases {
		refs := referencedIndexes(c.text)
		for _, w := range c.want {
			if !refs[w] {
				t.Fatalf("text %q: expected ref %d, got %v", c.text, w, refs)
			}
		}
		if len(refs) != len(c.want) {
			t.Fatalf("text %q: want %d refs, got %d (%v)", c.text, len(c.want), len(refs), refs)
		}
	}
}

func TestPruneToReferenced(t *testing.T) {
	cs := []SearchCitation{{Index: 1, Title: "a"}, {Index: 2, Title: "b"}, {Index: 3, Title: "c"}}

	// Only [1] and [3] cited → prune to those.
	got := pruneToReferenced(cs, "Answer draws on [1] and [3].")
	if len(got) != 2 || got[0].Index != 1 || got[1].Index != 3 {
		t.Fatalf("prune wrong: %+v", got)
	}

	// No recognizable citation → keep full list (so FE still shows sources).
	got = pruneToReferenced(cs, "Not enough information to answer.")
	if len(got) != 3 {
		t.Fatalf("no-citation case should keep all: %+v", got)
	}
}

func TestBuildAnswerContext(t *testing.T) {
	cs := []SearchCitation{
		{Index: 1, Source: UnifiedSourceWorkspace, Title: "Post by Alice", Meta: "#product", Snippet: "shipped the fix"},
		{Index: 2, Source: UnifiedSourceGitHub, Title: "Fix login", Meta: "o/r #1"},
	}
	out := buildAnswerContext("did the login fix ship?", cs)
	if !strings.Contains(out, "Question: did the login fix ship?") {
		t.Fatalf("missing question: %q", out)
	}
	if !strings.Contains(out, "[1] (Workspace) Post by Alice — #product") {
		t.Fatalf("missing workspace source line: %q", out)
	}
	if !strings.Contains(out, "shipped the fix") {
		t.Fatalf("missing snippet: %q", out)
	}
	if !strings.Contains(out, "[2] (GitHub) Fix login — o/r #1") {
		t.Fatalf("missing github source line: %q", out)
	}
}

func TestClipAnswer(t *testing.T) {
	long := strings.Repeat("word ", 200)
	out := clipAnswer(long)
	if len([]rune(out)) > answerCitationSnippet+1 {
		t.Fatalf("clip exceeded budget: %d", len([]rune(out)))
	}
}
