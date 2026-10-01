package codepr

import (
	"reflect"
	"testing"
)

func repos(names ...string) []RepoRef {
	out := make([]RepoRef, 0, len(names))
	for _, n := range names {
		// each name is "owner/name"
		for i := 0; i < len(n); i++ {
			if n[i] == '/' {
				out = append(out, RepoRef{Owner: n[:i], Name: n[i+1:]})
				break
			}
		}
	}
	return out
}

func TestMatchRepo_ExplicitFullName(t *testing.T) {
	rs := repos("akashc777/onecamp", "akashc777/onecamp-fe")
	got := matchRepo("please fix the bug in akashc777/onecamp-fe comment composer", rs)
	if !got.Found || got.Resolved.Name != "onecamp-fe" {
		t.Fatalf("expected onecamp-fe by full name, got %+v", got)
	}
}

func TestMatchRepo_BareNameWholeWord(t *testing.T) {
	rs := repos("akashc777/onecamp", "akashc777/onecamp-fe")
	// "onecamp-fe" must match onecamp-fe, NOT onecamp (substring guard).
	got := matchRepo("open a PR on onecamp-fe to fix padding", rs)
	if !got.Found || got.Resolved.Name != "onecamp-fe" {
		t.Fatalf("expected onecamp-fe, got %+v", got)
	}
	// "onecamp" as a whole word matches only onecamp.
	got = matchRepo("in onecamp, adjust the handler", rs)
	if !got.Found || got.Resolved.Name != "onecamp" {
		t.Fatalf("expected onecamp, got %+v", got)
	}
}

func TestMatchRepo_SingleLinkedShortcut(t *testing.T) {
	rs := repos("akashc777/onecamp")
	got := matchRepo("fix the flaky test", rs) // no repo named
	if !got.Found || got.Resolved.Name != "onecamp" {
		t.Fatalf("single linked repo should resolve implicitly, got %+v", got)
	}
}

func TestMatchRepo_AmbiguousNoMention(t *testing.T) {
	rs := repos("akashc777/onecamp", "akashc777/onecamp-fe")
	got := matchRepo("fix the flaky test", rs)
	if got.Found || len(got.Candidates) != 2 {
		t.Fatalf("no mention with multiple repos must be ambiguous, got %+v", got)
	}
}

func TestMatchRepo_MultipleNameMatches(t *testing.T) {
	rs := repos("akashc777/api", "other/web")
	got := matchRepo("touch both api and web", rs)
	if got.Found || len(got.Candidates) != 2 {
		t.Fatalf("two distinct name matches must be ambiguous, got %+v", got)
	}
}

func TestMatchRepo_NoneLinked(t *testing.T) {
	got := matchRepo("do something", nil)
	if got.Found || !got.NoneLinked {
		t.Fatalf("no linked repos must report NoneLinked, got %+v", got)
	}
}

func TestMatchRepo_DedupesDuplicates(t *testing.T) {
	rs := []RepoRef{{Owner: "a", Name: "r"}, {Owner: "a", Name: "r"}}
	got := matchRepo("no mention", rs)
	// After dedupe there's one repo → resolves implicitly.
	if !got.Found || got.Resolved.Name != "r" {
		t.Fatalf("duplicate linked repos should dedupe to one, got %+v", got)
	}
}

func TestContainsToken(t *testing.T) {
	if !containsToken("fix onecamp-fe now", "onecamp-fe") {
		t.Fatal("should match whole token")
	}
	if containsToken("fix onecamp-fe now", "onecamp") {
		t.Fatal("must not match a prefix inside a longer token")
	}
	if !containsToken("owner/name here", "owner/name") {
		t.Fatal("should match an owner/name unit")
	}
	if containsToken("", "x") || containsToken("text", "") {
		t.Fatal("empty inputs must not match")
	}
}

func TestCandidateNames(t *testing.T) {
	got := CandidateNames(repos("a/b", "c/d"))
	if !reflect.DeepEqual(got, []string{"a/b", "c/d"}) {
		t.Fatalf("unexpected candidate names: %v", got)
	}
}
