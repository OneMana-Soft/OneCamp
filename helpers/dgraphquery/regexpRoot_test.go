package dgraphquery

import (
	"strings"
	"testing"
)

func TestRegexpUnionRootUsesTheIndexOnlyWhenItCan(t *testing.T) {
	vars, root := RegexpUnionRoot(EscapeRegexLiteral("al"), "has(user_uuid)", "rx", []string{"user_name"})
	if vars != "" || root != "has(user_uuid)" {
		t.Fatalf("two letters can't use a trigram index: %q %q", vars, root)
	}
	vars, root = RegexpUnionRoot(EscapeRegexLiteral("a.b"), "has(user_uuid)", "rx", []string{"user_name", "user_full_name"}, "h")
	if root != "uid(rx0, rx1, h)" {
		t.Fatalf("root: %q", root)
	}
	for _, want := range []string{`rx0 as var(func: regexp(user_name, /.*a\.b.*/i))`, `rx1 as var(func: regexp(user_full_name, /.*a\.b.*/i))`} {
		if !strings.Contains(vars, want) {
			t.Fatalf("want %s in\n%s", want, vars)
		}
	}
	if !TrigramReady(`\.\.\.`) || TrigramReady(`\.\.`) || !TrigramReady("élé") {
		t.Fatal("literal characters are counted, escapes are not")
	}
}
