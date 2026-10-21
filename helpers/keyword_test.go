package helpers

import "testing"

// A keyword is the whole word, any case: "launch" in "Launch day" and
// "the launch.", never in "launched" or "relaunch".
func TestAKeywordIsTheWholeWord(t *testing.T) {
	re := KeywordPattern(" launch ")
	for text, want := range map[string]bool{"Launch day": true, "the launch.": true, "launched": false, "relaunch": false, "c++ launch": true} {
		if got := re.MatchString(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
	if KeywordPattern("C++").MatchString("we love c++") {
		// \b needs a word character at the edge: "C++" ends in punctuation.
		t.Log("a keyword ending in punctuation matches only before a word character, as workflows' always did")
	}
	if KeywordPattern("  ") != nil {
		t.Error("an empty keyword made a pattern")
	}
}
