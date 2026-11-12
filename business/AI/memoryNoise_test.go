package business

import (
	"fmt"
	"testing"
)

// V-581 (C11, R3): the extractor stored "Xnnnxxmmx", keyboard noise from a DM,
// as a commitment with a due date. A commitment is stored only when its words
// make one: a promise and what is promised. Noise, a single token, and text
// without words never are, whatever the model says of them, and an item keeps
// the model's own score.
func TestTheExtractorKeepsOnlyCommitmentsTheWordsMake(t *testing.T) {
	model := func(content string, confidence int) string {
		return fmt.Sprintf(`[{"kind":"commitment","content":%q,"owner":"Akash","due":"2026-06-08","confidence":%d}]`, content, confidence)
	}
	for _, noise := range []string{"Xnnnxxmmx", "ok", "lol", "👍", "...", "Akash Xnnnxxmmx", "will", "lol lol lol"} {
		if got := parseExtractedItems(model(noise, 100)); len(got) != 0 {
			t.Errorf("%q was kept as a commitment %+v; want nothing: it promises nothing", noise, got)
		}
	}
	for _, promise := range []string{"I'll send the deck by Friday", "Akash will send the launch deck to Maya", "Maya agreed to review the pricing page"} {
		got := parseExtractedItems(model(promise, 85))
		if len(got) != 1 || got[0].Content != promise || got[0].Confidence != 85 {
			t.Errorf("%q gave %+v; want it kept as a commitment, at the model's own 85", promise, got)
		}
	}
	if got := parseExtractedItems(model("I'll send the deck by Friday", 140)); len(got) != 1 || got[0].Confidence != 100 {
		t.Errorf("a score past 100 gave %+v; want the model's score held to 100", got)
	}
	if got := parseExtractedItems(model("I'll send the deck by Friday", -5)); len(got) != 1 || got[0].Confidence != 0 {
		t.Errorf("a score below 0 gave %+v; want it held to 0, never a made-up 70", got)
	}
}
