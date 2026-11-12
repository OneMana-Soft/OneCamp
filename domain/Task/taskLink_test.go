package Domain

import (
	"errors"
	"strings"
	"testing"
)

// A link names its pair once: relates from either side is one key, and a
// directed link reads from its from end; blocking is refused, pointing to
// dependencies.
func TestALinkKeysItsPairOnce(t *testing.T) {
	a, b := "B0000000-0000-0000-0000-000000000000", "a0000000-0000-0000-0000-000000000000"
	_, _, k1, err1 := LinkEnds(LinkRelates, a, b, "")
	_, _, k2, err2 := LinkEnds(LinkRelates, b, a, "in")
	if err1 != nil || err2 != nil || k1 != k2 || k1 != "relates:a0000000-0000-0000-0000-000000000000:b0000000-0000-0000-0000-000000000000" {
		t.Errorf("relates keys %q and %q (%v %v), want one key in order", k1, k2, err1, err2)
	}
	from, to, k, err := LinkEnds(LinkDuplicates, a, b, "")
	if err != nil || from != strings.ToLower(a) || to != b || k != "duplicates:"+strings.ToLower(a)+":"+b {
		t.Errorf("a duplicates b: %s -> %s %q %v", from, to, k, err)
	}
	from, to, k, err = LinkEnds(LinkClones, a, b, "in")
	if err != nil || from != b || to != strings.ToLower(a) || k != "clones:"+b+":"+strings.ToLower(a) {
		t.Errorf("a is cloned by b: %s -> %s %q %v", from, to, k, err)
	}
	for _, c := range []struct{ kind, other, words string }{
		{"blocks", b, "Dependencies"},
		{"follows", b, "relates, duplicates or clones"},
		{LinkRelates, strings.ToLower(a), "itself"},
	} {
		if _, _, _, err := LinkEnds(c.kind, a, c.other, ""); !errors.Is(err, ErrLink) || !strings.Contains(err.Error(), c.words) {
			t.Errorf("%s: %v, want a refusal saying %q", c.kind, err, c.words)
		}
	}
	if err := LinkKind("blocks"); !errors.Is(err, ErrLink) || LinkKind(LinkClones) != nil {
		t.Errorf("kinds: blocks %v, clones %v", err, LinkKind(LinkClones))
	}
}

// Each side reads a link in its own words.
func TestALinkReadsFromItsSide(t *testing.T) {
	for _, c := range []struct{ kind, dir, want string }{
		{LinkRelates, "out", "relates to"}, {LinkRelates, "in", "relates to"},
		{LinkDuplicates, "out", "duplicates"}, {LinkDuplicates, "in", "is duplicated by"},
		{LinkClones, "out", "clones"}, {LinkClones, "in", "is cloned by"},
	} {
		if got := LinkLabel(c.kind, c.dir); got != c.want {
			t.Errorf("%s %s reads %q, want %q", c.kind, c.dir, got, c.want)
		}
	}
}
