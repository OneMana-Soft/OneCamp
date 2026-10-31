package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// The chip's link family answers as the web's linkProvider family does, case
// by case from testdata/linkfamily/links.json, which the web keeps byte for
// byte in its own testdata (lib/docs/docNode.test.ts reads it there).
func TestALinksFamilyIsTheWebs(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/linkfamily/links.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ URL, Family string }
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases: the fixture proves nothing")
	}
	for _, c := range cases {
		if got := LinkFamily(c.URL); got != c.Family {
			t.Errorf("LinkFamily(%q) = %q, want %q", c.URL, got, c.Family)
		}
	}
}
