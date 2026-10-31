package business

import (
	"encoding/json"
	"testing"
)

// Tables B5c (main, 28 Oct): a link hides a shown question only where the
// form's managers let it, question by question. The mark is kept with the
// form, never on a section or a file question, and a question hidden on every
// visit doesn't need it.
func TestALinkHidesOnlyTheQuestionsItsManagersMark(t *testing.T) {
	f, err := CheckForm(Input{Title: "Vendor sign-up", Fields: []Field{
		{Id: "src", Label: "Source", Type: ShortText, LinkHides: true},
		{Id: "agree", Label: "I agree to the terms", Type: Checkbox, Required: true},
		{Id: "ref", Label: "Referrer", Type: ShortText, Hidden: true, LinkHides: true},
	}})
	if err != nil {
		t.Fatalf("a marked question: %v", err)
	}
	var kept []Field
	if err := json.Unmarshal(f.Fields, &kept); err != nil {
		t.Fatal(err)
	}
	if !kept[0].LinkHides || kept[1].LinkHides || kept[2].LinkHides {
		t.Errorf("kept marks %v %v %v, want true false false (a hidden question is hidden on every visit already)", kept[0].LinkHides, kept[1].LinkHides, kept[2].LinkHides)
	}
	for _, bad := range []Field{{Id: "s", Label: "Page two", Type: Section, LinkHides: true}, {Id: "d", Label: "Brief", Type: File, LinkHides: true}} {
		if _, err := CheckForm(Input{Title: "x", Fields: []Field{bad, {Id: "q", Label: "Name", Type: ShortText}}}); err == nil {
			t.Errorf("%s marked for a link to hide: saved, want refused", bad.Type)
		}
	}
}
