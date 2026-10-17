package business

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// longDoc is a doc of n sections, each with a fact of its own at its end.
func longDoc(n, wordsPerSection int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "## Section %d\n\n", i)
		for w := 0; w < wordsPerSection; w++ {
			b.WriteString("filler ")
			if w%60 == 59 {
				b.WriteString("\n\n")
			}
		}
		fmt.Fprintf(&b, "\n\nFact %d: the budget for part %d is %d pounds.\n\n", i, i, i*1000)
	}
	return b.String()
}

func TestSplitDocForSummaryKeepsEverySection(t *testing.T) {
	limits := ai.ModelLimits{ContextWindow: 8192}
	md := longDoc(12, 420) // about 5,000 words
	parts := SplitDocForSummary(md, limits, 1500)
	if len(parts) < 2 {
		t.Fatalf("a 5,000-word doc came out as %d part(s)", len(parts))
	}
	joined := strings.Join(parts, "\n")
	for i := 1; i <= 12; i++ {
		if !strings.Contains(joined, fmt.Sprintf("Fact %d:", i)) {
			t.Errorf("section %d's fact is in no part", i)
		}
	}
	for i, p := range parts {
		if limits.EstimateTokens(p) > 1500 {
			t.Errorf("part %d is %d tokens, over 1,500", i, limits.EstimateTokens(p))
		}
	}
}

// One paragraph longer than a part is split at its sentences, and nothing in
// it goes unread.
func TestALongParagraphIsSplitNotCut(t *testing.T) {
	limits := ai.ModelLimits{ContextWindow: 8192}
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "Sentence number %d says something. ", i)
	}
	para := strings.TrimSpace(b.String())
	parts := splitLongParagraph(para, limits, 300)
	if len(parts) < 2 {
		t.Fatalf("got %d part(s)", len(parts))
	}
	if strings.Join(strings.Fields(strings.Join(parts, " ")), " ") != strings.Join(strings.Fields(para), " ") {
		t.Error("the parts don't add up to the paragraph")
	}
	// A sentence longer than a part is cut by length, and kept whole across the cuts.
	huge := strings.Repeat("x", 5000)
	if got := strings.Join(splitLongParagraph(huge, limits, 100), ""); got != huge {
		t.Errorf("a sentence with no ends lost %d characters", len(huge)-len(got))
	}
}

// The summary of a long doc is written from every part: a fact from the last
// section reaches the step that writes it (ai-search-io P0-4).
func TestASummaryOfALongDocReadsItsLastSection(t *testing.T) {
	limits := ai.ModelLimits{ContextWindow: 8192}
	md := longDoc(12, 420)
	parts, mapped, cut := PlanDocSummary(md, limits, 2000)
	if !mapped || cut {
		t.Fatalf("mapped=%v cut=%v for %d tokens", mapped, cut, limits.EstimateTokens(md))
	}
	saved := chatForSummary
	t.Cleanup(func() { chatForSummary = saved })
	calls := 0
	chatForSummary = func(_ context.Context, _ ai.LLMProvider, msgs []ai.ChatMessage, _ ai.ChatOptions) (string, error) {
		calls++
		// A model that keeps each part's facts, with a line of its own around them.
		var facts []string
		for _, l := range strings.Split(msgs[1].Content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "Fact ") || strings.HasPrefix(strings.TrimSpace(l), "- Fact ") {
				facts = append(facts, "- "+strings.TrimPrefix(strings.TrimSpace(l), "- "))
			}
		}
		return "Here is the summary of this part:\n\n" + strings.Join(facts, "\n") + "\n\nLet me know if you need more.", nil
	}
	progress := 0
	joined, err := SummariseDocParts(context.Background(), nil, "Budget", parts, limits, 2000, func(done, of int) { progress++ })
	if err != nil {
		t.Fatal(err)
	}
	if calls != len(parts) || progress != len(parts) {
		t.Errorf("calls=%d progress=%d for %d parts", calls, progress, len(parts))
	}
	if !strings.Contains(joined, "Fact 12: the budget for part 12 is 12000 pounds.") {
		t.Errorf("the last section's fact didn't reach the summary step:\n%s", joined)
	}
	if strings.Contains(joined, "Here is the summary") || strings.Contains(joined, "Let me know") {
		t.Errorf("a part's preamble reached the summary step:\n%s", joined)
	}
}

// When the parts' summaries together are still too long to read at once, they
// are summarised together again, so the summary is never written from a cut.
func TestPartSummariesAreCondensedUntilTheyFit(t *testing.T) {
	limits := ai.ModelLimits{ContextWindow: 8192}
	parts := make([]string, 20)
	for i := range parts {
		parts[i] = fmt.Sprintf("part %d", i+1)
	}
	saved := chatForSummary
	t.Cleanup(func() { chatForSummary = saved })
	calls := 0
	chatForSummary = func(_ context.Context, _ ai.LLMProvider, _ []ai.ChatMessage, _ ai.ChatOptions) (string, error) {
		calls++
		return strings.TrimSpace(strings.Repeat("summary words ", 60)), nil
	}
	joined, err := SummariseDocParts(context.Background(), nil, "", parts, limits, 900, nil)
	if err != nil {
		t.Fatal(err)
	}
	headed := strings.Count(joined, "Part ")
	if calls <= len(parts) || headed >= len(parts) {
		t.Errorf("the summaries were not condensed: %d calls for %d parts, %d summaries left", calls, len(parts), headed)
	}
	if !strings.HasPrefix(joined, "Part 1:\n") {
		t.Errorf("the summaries are not headed by their part: %q", joined[:20])
	}
}

func TestAShortDocIsSummarisedInOneGo(t *testing.T) {
	if parts, mapped, cut := PlanDocSummary("## Plan\n\nShip on Friday.", ai.ModelLimits{ContextWindow: 8192}, 2000); mapped || cut || parts != nil {
		t.Errorf("a short doc was split: %v %v %v", parts, mapped, cut)
	}
}
