package codepr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// TestMain wires a discard logger so functions that log on a degraded path
// (e.g. resolveExplicitUnlinked's verify-error branch) don't panic on the nil
// global logger under unit tests.
func TestMain(m *testing.M) {
	if helpers.Logger == nil {
		helpers.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	os.Exit(m.Run())
}

func TestParseExplicitRepoRefs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []RepoRef
	}{
		{"bare slug", "open a PR on akashc777/first_nodejs to add tests",
			[]RepoRef{{"akashc777", "first_nodejs"}}},
		{"https url", "fix the bug at https://github.com/octocat/Hello-World please",
			[]RepoRef{{"octocat", "Hello-World"}}},
		{"url with pull path", "see github.com/octocat/hello.git/pull/3",
			[]RepoRef{{"octocat", "hello"}}},
		{"trailing punctuation", "work on akashc777/repo.",
			[]RepoRef{{"akashc777", "repo"}}},
		{"dedupe", "akashc777/repo and again akashc777/repo",
			[]RepoRef{{"akashc777", "repo"}}},
		{"none", "just fix the flaky test", nil},
		{"multi-slash path is not a repo", "edit the file src/app/main.go now", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseExplicitRepoRefs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseExplicitRepoRefs(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestResolveExplicit_AccessibleUnlinked(t *testing.T) {
	linked := repos("akashc777/onecamp")
	verify := func(_ context.Context, owner, name string) (bool, error) {
		if owner == "akashc777" && name == "first_nodejs" {
			return true, nil
		}
		return false, nil
	}
	res, handled := resolveExplicit(context.Background(),
		"open a PR on akashc777/first_nodejs", linked, true, verify)
	if !handled {
		t.Fatal("should take ownership of an explicit non-linked repo")
	}
	if !res.Found || res.Resolved.Name != "first_nodejs" {
		t.Fatalf("expected first_nodejs resolved, got %+v", res)
	}
}

func TestResolveExplicit_InaccessibleUnlinked(t *testing.T) {
	verify := func(_ context.Context, _, _ string) (bool, error) { return false, nil }
	res, handled := resolveExplicit(context.Background(),
		"open a PR on ghost/missing", repos("a/b"), true, verify)
	if !handled || res.Found {
		t.Fatalf("inaccessible repo should be handled + not found, got handled=%v res=%+v", handled, res)
	}
	if res.InaccessibleRepo.FullName() != "ghost/missing" {
		t.Fatalf("expected InaccessibleRepo ghost/missing, got %+v", res.InaccessibleRepo)
	}
}

// The regression from the prod log: an explicitly-named UNLINKED repo while the
// feature is OFF must be surfaced (UnlinkedDisabled), NEVER silently resolved to
// a different linked repo.
func TestResolveExplicit_UnlinkedDisabledIsAuthoritative(t *testing.T) {
	called := false
	verify := func(_ context.Context, _, _ string) (bool, error) { called = true; return true, nil }
	res, handled := resolveExplicit(context.Background(),
		"create PR in https://github.com/akashc777/first_nodejs with updated readme",
		repos("akashc777/OneCamp-fe-test"), false, verify)
	if !handled || res.Found {
		t.Fatalf("named unlinked repo (feature off) must be handled + not found, got handled=%v res=%+v", handled, res)
	}
	if res.UnlinkedDisabled.FullName() != "akashc777/first_nodejs" {
		t.Fatalf("expected UnlinkedDisabled first_nodejs, got %+v", res.UnlinkedDisabled)
	}
	if called {
		t.Fatal("must not verify access when the feature is off")
	}
}

func TestResolveExplicit_DefersWhenLinkedNamed(t *testing.T) {
	called := false
	verify := func(_ context.Context, _, _ string) (bool, error) { called = true; return true, nil }
	_, handled := resolveExplicit(context.Background(),
		"fix akashc777/onecamp handler", repos("akashc777/onecamp"), true, verify)
	if handled {
		t.Fatal("a named LINKED repo must defer to matchRepo (handled=false)")
	}
	if called {
		t.Fatal("must not verify access for a linked repo")
	}
}

func TestResolveExplicit_DefersWhenNoExplicit(t *testing.T) {
	_, handled := resolveExplicit(context.Background(),
		"just fix the flaky test", repos("a/b"), true, func(_ context.Context, _, _ string) (bool, error) {
			return true, nil
		})
	if handled {
		t.Fatal("no explicit repo named must defer to matchRepo")
	}
}

func TestResolveExplicit_VerifyErrorDegradesToAsk(t *testing.T) {
	verify := func(_ context.Context, _, _ string) (bool, error) { return false, errors.New("boom") }
	res, handled := resolveExplicit(context.Background(),
		"open a PR on ghost/missing", repos("a/b", "c/d"), true, verify)
	if !handled || res.Found {
		t.Fatalf("verify error should be handled + not found, got handled=%v res=%+v", handled, res)
	}
	if len(res.Candidates) != 2 || res.InaccessibleRepo.Valid() {
		t.Fatalf("verify error should ask with candidates, no inaccessible flag, got %+v", res)
	}
}

func TestResolveExplicit_NilVerifyDegradesToAsk(t *testing.T) {
	res, handled := resolveExplicit(context.Background(),
		"open a PR on ghost/missing", repos("a/b"), true, nil)
	if !handled || res.Found {
		t.Fatalf("nil verify should be handled + not found, got handled=%v res=%+v", handled, res)
	}
}

func TestResolveExplicit_PrefersLinkedWhenBothNamed(t *testing.T) {
	verify := func(_ context.Context, _, _ string) (bool, error) { return true, nil }
	_, handled := resolveExplicit(context.Background(),
		"sync ghost/missing into akashc777/onecamp", repos("akashc777/onecamp"), true, verify)
	if handled {
		t.Fatal("when a linked repo is also named, must defer to matchRepo")
	}
}
