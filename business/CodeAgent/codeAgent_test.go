package codeagent

import "testing"

func TestExtractCandidatePaths(t *testing.T) {
	text := `Traceback: in src/app/main.go:42 and also web/components/Login.tsx.
See package-lock.json and node_modules/foo/index.js. Version v1.2 is fine. README mentioned.`
	got := extractCandidatePaths(text)

	want := map[string]bool{
		"src/app/main.go":           true,
		"web/components/Login.tsx":  true,
		"node_modules/foo/index.js": true,
		"package-lock.json":         true,
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path extracted: %q", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("expected path not extracted: %q", p)
	}
	// "v1.2" has a numeric-only extension token that isn't a code ext → excluded.
	for _, p := range got {
		if p == "v1.2" {
			t.Errorf("v1.2 should not be treated as a path")
		}
	}
}

func TestIsVendored(t *testing.T) {
	cases := map[string]bool{
		"node_modules/react/index.js": true,
		"vendor/github.com/x/y.go":    true,
		"dist/bundle.js":              true,
		"app/main.min.js":             true,
		"go.sum":                      true,
		"pkg/auth/login.go":           false,
		"src/components/Login.tsx":    false,
		"api/gen/proto.pb.go":         true,
		"internal/testdata/sample.go": true,
	}
	for path, want := range cases {
		if got := isVendored(path); got != want {
			t.Errorf("isVendored(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsSourcePath(t *testing.T) {
	cases := map[string]bool{
		"a/b.go":    true,
		"a/b.tsx":   true,
		"a/b.py":    true,
		"README.md": false,
		"img/a.png": false,
		"noext":     false,
		"trailing.": false,
	}
	for path, want := range cases {
		if got := isSourcePath(path); got != want {
			t.Errorf("isSourcePath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIssueTokens(t *testing.T) {
	toks := issueTokens("Login button crashes with a NullPointer error in AuthService")
	has := func(w string) bool {
		for _, t := range toks {
			if t == w {
				return true
			}
		}
		return false
	}
	if !has("login") || !has("authservice") || !has("nullpointer") {
		t.Errorf("expected key tokens present, got %v", toks)
	}
	// stopwords filtered.
	if has("with") || has("error") {
		t.Errorf("stopwords should be filtered, got %v", toks)
	}
}

func TestRankPathsByTokens(t *testing.T) {
	paths := []string{
		"src/auth/AuthService.go",
		"src/util/strings.go",
		"src/auth/login.go",
		"node_modules/auth/index.js", // vendored → dropped
		"docs/auth.md",               // not a source ext → dropped
	}
	tokens := []string{"auth", "login", "authservice"}
	got := rankPathsByTokens(paths, tokens)

	if len(got) == 0 {
		t.Fatal("expected ranked results")
	}
	// Highest scorer should be the AuthService basename (multi-token basename hit).
	if got[0] != "src/auth/AuthService.go" {
		t.Errorf("expected AuthService.go ranked first, got %v", got)
	}
	for _, p := range got {
		if p == "node_modules/auth/index.js" {
			t.Error("vendored path should be excluded from ranking")
		}
		if p == "docs/auth.md" {
			t.Error("non-source path should be excluded from ranking")
		}
	}
}
