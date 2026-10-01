package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// childenv_test.go — the child-process environment invariants. Every test here
// pins a rule that, when broken, produced a failure that looked like something
// else entirely: a feature that worked in dev and could not reach the network in
// the hardened deployment, or a PR polluted with build scratch files.

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"="), true
		}
	}
	return "", false
}

// TestPassthroughEnv_ForwardsOnlyWhatIsNamedAndSet is the generic contract: named
// + set is forwarded, named + unset is skipped, and nothing else is inherited —
// so the runner's own secrets can never ride along.
func TestPassthroughEnv_ForwardsOnlyWhatIsNamedAndSet(t *testing.T) {
	t.Setenv("ONECAMP_TEST_FORWARD", "yes")
	t.Setenv("ONECAMP_TEST_BLANK", "   ")
	os.Unsetenv("ONECAMP_TEST_ABSENT")

	got := passthroughEnv("ONECAMP_TEST_FORWARD", "ONECAMP_TEST_ABSENT", "ONECAMP_TEST_BLANK")
	if v, ok := envValue(got, "ONECAMP_TEST_FORWARD"); !ok || v != "yes" {
		t.Fatalf("a named, set variable must be forwarded; got %q", got)
	}
	if _, ok := envValue(got, "ONECAMP_TEST_ABSENT"); ok {
		t.Fatal("an unset variable must not be forwarded")
	}
	if _, ok := envValue(got, "ONECAMP_TEST_BLANK"); ok {
		t.Fatal("a blank variable must not be forwarded (it would override nothing usefully)")
	}
	if len(got) != 1 {
		t.Fatalf("nothing beyond the named variables may be inherited; got %v", got)
	}
}

// TestGitEnv_CarriesTheEgressRoute is the regression test for the bug that made
// the whole feature inert in its own shipped topology: the runner builds a
// secret-free env for git from scratch, which also dropped HTTP(S)_PROXY — the
// ONLY route to the git host on an internal-only network. Every clone and push
// failed with a network error, and the containment smoke check never caught it
// because it exercises the proxy from a separate container.
func TestGitEnv_CarriesTheEgressRoute(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://egress-proxy:8888")
	t.Setenv("https_proxy", "http://egress-proxy:8888")
	t.Setenv("NO_PROXY", "go-service,localhost")

	env := gitEnv("/work/repo")
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY"} {
		if _, ok := envValue(env, key); !ok {
			t.Fatalf("git must inherit %s or it cannot reach the git host through the allowlisting proxy; env=%v", key, env)
		}
	}
	// The hardening that motivated the explicit env must still hold.
	if v, _ := envValue(env, "GIT_TERMINAL_PROMPT"); v != "0" {
		t.Fatal("git must stay non-interactive so a missing token fails fast")
	}
	if v, _ := envValue(env, "GIT_CONFIG_GLOBAL"); v != "/dev/null" {
		t.Fatal("git must ignore any global config")
	}
}

// TestVerifierEnv_CarriesTheEgressRoute pins the same rule for build/test gates:
// without it, dependency resolution cannot reach an allowlisted package registry,
// so verification fails on every repo that has external dependencies.
func TestVerifierEnv_CarriesTheEgressRoute(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://egress-proxy:8888")
	env := verifierEnv("/work/repo", "/work/verifier-tmp")
	if _, ok := envValue(env, "HTTPS_PROXY"); !ok {
		t.Fatalf("a build gate must inherit the egress route; env=%v", env)
	}
	if v, _ := envValue(env, "TMPDIR"); v != "/work/verifier-tmp" {
		t.Fatalf("the gate must use the scratch TMPDIR it was given; got %q", v)
	}
}

// TestChildEnv_NeverLeaksRunnerSecrets guards the reason the env is explicit in
// the first place: the runner's own tokens must not reach a build script or a
// test file the model wrote.
func TestChildEnv_NeverLeaksRunnerSecrets(t *testing.T) {
	t.Setenv("CODE_RUNNER_TOKEN", "super-secret-shared-token")
	t.Setenv("HTTPS_PROXY", "http://egress-proxy:8888")

	for name, env := range map[string][]string{
		"gitEnv":      gitEnv("/work/repo"),
		"verifierEnv": verifierEnv("/work/repo", "/work/verifier-tmp"),
	} {
		for _, kv := range env {
			if strings.Contains(kv, "super-secret-shared-token") {
				t.Fatalf("%s leaked the runner token into a child process: %q", name, kv)
			}
		}
	}
}

// TestVerifierTmpDir_SitsOutsideTheWorkTree is the regression test for scratch
// files landing in the pull request: finalization stages with `git add -A`, so a
// TMPDIR inside the checkout meant any residue a gate left behind — routine when
// a gate is killed at its deadline — was committed and published.
func TestVerifierTmpDir_SitsOutsideTheWorkTree(t *testing.T) {
	root := "/work/coderun-123/repo"
	got := verifierTmpDir(root)
	if strings.HasPrefix(filepath.Clean(got)+string(filepath.Separator), filepath.Clean(root)+string(filepath.Separator)) {
		t.Fatalf("the scratch dir must not sit inside the git work tree, or add -A commits it; got %q for root %q", got, root)
	}
	// Still inside the per-run temp dir, so it is wiped with the run.
	if !strings.HasPrefix(got, "/work/coderun-123") {
		t.Fatalf("the scratch dir must stay under the per-run temp dir so it is cleaned up; got %q", got)
	}
}

// TestDiffArgs_ReadTheIndexNotTheWorkTree is the hermetic half of the new-file
// regression (the behavioural half needs real git and lives in the tagged
// integration tests). `git diff HEAD` compares the WORK TREE to HEAD, and an
// untracked file appears in neither — so measuring the change that way counted a
// new-files-only change as zero, and the run reported "I couldn't produce a
// change", pushed nothing, and deleted the checkout. Reading the INDEX after
// staging is what makes a created file visible.
func TestDiffArgs_ReadTheIndexNotTheWorkTree(t *testing.T) {
	for name, args := range map[string][]string{
		"diffArgs":     diffArgs(),
		"diffStatArgs": diffStatArgs(),
	} {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--cached") {
			t.Errorf("%s must read the index (--cached) so newly created files are counted; got %q", name, joined)
		}
		if strings.Contains(joined, "HEAD") {
			t.Errorf("%s must not diff the work tree against HEAD — that is invisible to untracked files; got %q", name, joined)
		}
	}
	if strings.Join(stageArgs(), " ") != "add -A" {
		t.Fatalf("staging must pick up every change including new files; got %q", stageArgs())
	}
}
