package codepr

import (
	"strings"
	"testing"
)

func TestSanitize_RedactsTokens(t *testing.T) {
	if got := Sanitize("bearer abcdefghijklmnopqrstuvwxyz012345"); strings.Contains(got, "abcdefghij") {
		t.Fatalf("bearer token not redacted: %q", got)
	}
	if got := Sanitize("x-access-token:supersecret@github.com"); strings.Contains(got, "supersecret") {
		t.Fatalf("x-access-token not redacted: %q", got)
	}
	if got := Sanitize("fatal: auth failed for ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"); strings.Contains(got, "ghp_ABCDEF") {
		t.Fatalf("PAT not redacted: %q", got)
	}
}

// TestSanitize_StripsTheRunnersRealCheckoutPaths pins path redaction against the
// layout the runner ACTUALLY creates. The original regex expected the per-run
// directory to have a hex-ish name, but the runner builds it with
// MkdirTemp(workRoot(), "coderun-"), so nothing ever matched and every build
// message published the sandbox's filesystem layout. The rule was untested, which
// is exactly why it could be wrong for so long.
func TestSanitize_StripsTheRunnersRealCheckoutPaths(t *testing.T) {
	cases := map[string]string{
		"/work/coderun-1234567890/repo/main.go:12: undefined: foo": "main.go:12: undefined: foo",
		"/work/coderun-9f2ab1c4d5e6/repo/pkg/svc/a.go:3:1: oops":   "pkg/svc/a.go:3:1: oops",
		"/work/coderun-42/verifier-tmp/go-build/x.o: no such file": "go-build/x.o: no such file",
	}
	for in, want := range cases {
		got := Sanitize(in)
		if got != want {
			t.Errorf("Sanitize(%q)\n got: %q\nwant: %q", in, got, want)
		}
		if strings.Contains(got, "coderun-") || strings.Contains(got, "/work/") {
			t.Errorf("the sandbox layout must not survive sanitization; got %q", got)
		}
	}
}

// TestSanitize_LeavesOrdinaryTextAlone guards against over-redaction: a summary a
// reviewer needs must not be chewed up by the path rules.
func TestSanitize_LeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"main.go:12: undefined: foo",
		"FAIL github.com/acme/svc/handlers 0.31s",
		"Expected 3 to equal 4",
		"src/components/Button.tsx(14,5): error TS2322",
		"npm ERR! Missing script: \"build\"",
	} {
		if got := Sanitize(in); got != in {
			t.Errorf("ordinary output must pass through unchanged;\n in: %q\nout: %q", in, got)
		}
	}
}
