package business

import (
	"reflect"
	"testing"
)

func TestParseAmbientKeywords(t *testing.T) {
	got := parseAmbientKeywords("Billing, refund\ninvoice ; Billing\n\n  ")
	want := []string{"billing", "refund", "invoice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAmbientKeywords = %v, want %v", got, want)
	}
	if len(parseAmbientKeywords("")) != 0 {
		t.Fatalf("empty should yield no keywords")
	}
}

func TestAmbientCandidate(t *testing.T) {
	kw := []string{"billing", "refund"}

	// Questions always qualify (even with no keywords).
	if !ambientCandidate("how do I do this?", nil) {
		t.Fatalf("a question should qualify with no keywords")
	}
	// A statement with no keywords does NOT qualify (conservative default).
	if ambientCandidate("shipping it now", nil) {
		t.Fatalf("a plain statement with no keywords should not qualify")
	}
	// Keyword match qualifies (case-insensitive).
	if !ambientCandidate("the Billing looks off", kw) {
		t.Fatalf("a keyword match should qualify")
	}
	// No keyword, no question → does not qualify.
	if ambientCandidate("the weather is nice", kw) {
		t.Fatalf("no keyword + no question should not qualify")
	}
	// Blank / whitespace never qualifies.
	for _, s := range []string{"", "   ", "\n\t"} {
		if ambientCandidate(s, kw) {
			t.Fatalf("blank %q should never qualify", s)
		}
	}
}
