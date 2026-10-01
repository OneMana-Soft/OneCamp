package domain

import (
	"strings"
	"testing"
)

// The needle is interpolated into /.*NEEDLE.*/i. These are the ways that goes
// wrong, and one caller turns the result into a DM recipient.
func TestEscapeForDgraphRegex(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		// what must NOT survive unescaped into the pattern
		mustNotContainRaw string
	}{
		// Closes the regex literal, so everything after it lands in the filter
		// expression rather than in the pattern.
		{name: "slash closes the literal", in: "a/b", mustNotContainRaw: "a/b"},
		// Widens the match: ".*" against a name filter matches everybody, and the
		// caller takes found[0].
		{name: "wildcard widens the match", in: ".*", mustNotContainRaw: ".*.*"},
		// Alternation smuggles a second name in.
		{name: "alternation", in: "priya|admin", mustNotContainRaw: "priya|admin"},
		// Anchors and groups change the semantics of the surrounding pattern.
		{name: "anchors", in: "^root$", mustNotContainRaw: "^root$"},
		{name: "grouping", in: "(a)(b)", mustNotContainRaw: "(a)(b)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeForDgraphRegex(tc.in)
			if strings.Contains(got, tc.mustNotContainRaw) {
				t.Errorf("escapeForDgraphRegex(%q) = %q, which still carries %q into the pattern",
					tc.in, got, tc.mustNotContainRaw)
			}
		})
	}
}

// An ordinary name has to keep working, or the mention typeahead breaks.
func TestOrdinaryNamesSurvive(t *testing.T) {
	for _, in := range []string{"priya", "Priya Sharma", "o'brien", "jean-luc"} {
		got := escapeForDgraphRegex(in)
		if got == "" {
			t.Errorf("%q escaped to nothing", in)
		}
		// Letters must pass through untouched or the search stops matching.
		if !strings.Contains(strings.ToLower(got), strings.ToLower(strings.Fields(in)[0])) {
			t.Errorf("escapeForDgraphRegex(%q) = %q lost the name itself", in, got)
		}
	}
}

// A very long needle is a way to make the store work hard for nothing.
func TestLongNeedleIsBounded(t *testing.T) {
	got := escapeForDgraphRegex(strings.Repeat("a", 5000))
	if len(got) > searchTextMaxLen*2 {
		t.Errorf("a 5000-char needle produced %d chars", len(got))
	}
}

// Whitespace-only input should not become a pattern that matches everything.
func TestBlankNeedle(t *testing.T) {
	if got := escapeForDgraphRegex("   "); got != "" {
		t.Errorf("blank needle became %q", got)
	}
}

// The escaping is worthless if the query interpolates the raw needle anyway.
// This is the wiring, which a unit test of the helper cannot see.
func TestQueryUsesTheEscapedNeedle(t *testing.T) {
	src := readUserDomainSource(t)
	i := strings.Index(src, "func GetUserListWithSearchText")
	if i < 0 {
		t.Fatal("search function not found")
	}
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "escapeForDgraphRegex(searchText)") {
		t.Fatal("the needle is no longer escaped before the query is built")
	}
	if strings.Contains(body, "`, searchText)") {
		t.Error("the query still interpolates the RAW needle")
	}
}
