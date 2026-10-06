package helpers

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormaliseTags(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"bug":                               "bug",
		" frontend ,  needs   review, ,Bug": "frontend, needs review, Bug",
		"Bug, bug, BUG":                     "Bug",
		strings.Repeat("x", 40):             strings.Repeat("x", MaxTagLength),
		"a,b,c,d,e,f,g,h,i,j,k,l":           "a, b, c, d, e, f, g, h, i, j",
	}
	for in, want := range cases {
		if got := NormaliseTags(in); got != want {
			t.Errorf("NormaliseTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTagChanges(t *testing.T) {
	added, removed := TagChanges("bug, backend", "Bug, frontend")
	if !reflect.DeepEqual(added, []string{"frontend"}) || !reflect.DeepEqual(removed, []string{"backend"}) {
		t.Errorf("added %v removed %v", added, removed)
	}
}

func TestWithAndWithoutTag(t *testing.T) {
	if got := WithTag("bug", "frontend"); got != "bug, frontend" {
		t.Errorf("WithTag = %q", got)
	}
	if got := WithTag("bug", "BUG"); got != "bug" {
		t.Errorf("WithTag twice = %q", got)
	}
	if got := WithoutTag("bug, frontend", "Bug"); got != "frontend" {
		t.Errorf("WithoutTag = %q", got)
	}
}
