package business

import (
	"strings"
	"testing"
)

func TestScalarToString(t *testing.T) {
	cases := map[string]struct {
		in   interface{}
		want string
	}{
		"string":       {"hello", "hello"},
		"int":          {42, "42"},
		"float whole":  {float64(7), "7"},
		"float frac":   {float64(3.5), "3.5"},
		"bool":         {true, "true"},
		"nested empty": {map[string]any{"a": 1}, ""},
	}
	for name, c := range cases {
		if got := scalarToString(c.in); got != c.want {
			t.Errorf("%s: scalarToString(%v) = %q, want %q", name, c.in, got, c.want)
		}
	}
}

func TestEventContextLinesGitHub(t *testing.T) {
	// A github.check_run.completed payload (pr_number arrives as int) must render
	// the repo + PR context so a PR-follow agent has something to act on.
	data := map[string]interface{}{
		"owner":      "acme",
		"repo":       "api",
		"pr_number":  482,
		"pr_url":     "https://github.com/acme/api/pull/482",
		"conclusion": "success",
		"check_name": "ci/tests",
	}
	got := eventContextLines(data)
	for _, want := range []string{"owner: acme", "repo: api", "pr_number: 482", "conclusion: success"} {
		if !strings.Contains(got, want) {
			t.Errorf("eventContextLines missing %q in:\n%s", want, got)
		}
	}
}

func TestEventContextLinesCapsLongValue(t *testing.T) {
	long := strings.Repeat("x", eventFieldMaxLen+50)
	got := eventContextLines(map[string]interface{}{"body": long})
	if !strings.HasSuffix(strings.TrimSpace(got), "…") {
		t.Errorf("long value should be capped + ellipsized, got tail: %q", got[len(got)-5:])
	}
}
